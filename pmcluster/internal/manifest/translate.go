package manifest

import (
	"fmt"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-stack/pmcluster/pkg/dsl"
)

// Standard external resources every translated stack expects to exist.
// Created by `pmcluster cluster up`; the translator emits `external: true`
// references so `docker stack deploy` doesn't try to re-create them.
const (
	traefikNet    = "traefik-net"
	monitoringNet = "monitoring-net"
)

// privateNetSuffix is appended to App.Name to form the per-app overlay
// network where the app's own services talk to each other (e.g.
// "donation-campaign-net"). Inline overlay (not external).
const privateNetSuffix = "-net"

// labelKeyPrefix etc. — the standard label set the translator stamps on
// every service. Operators can grep their cluster for these to find
// pmcluster-managed services.
const (
	labelService     = "service"
	labelApplication = "application"
	labelEnvironment = "environment"
	labelVersion     = "version"
)

// Translate renders a validated, interpolated *dsl.App into compose v3.9
// YAML bytes. The output is stable (deterministic field ordering, alpha
// map sort) so golden-file tests are practical.
//
// CALLER MUST run Parse → Interpolate → Validate first; Translate does no
// validation itself. Calling Translate on an invalid manifest produces
// invalid compose, which `docker stack deploy` will reject loudly.
func Translate(app *dsl.App) ([]byte, error) {
	cf := &composeFile{
		Version:  "3.9",
		Services: map[string]*composeService{},
	}

	privateNet := app.Name + privateNetSuffix

	// Track which shared networks we end up using; we only emit them in the
	// top-level `networks:` block if at least one service consumes them.
	usesTraefikNet := false
	usesMonitoringNet := false

	for name, svc := range app.Services {
		cs := translateService(app, name, svc, privateNet, &usesTraefikNet, &usesMonitoringNet)
		cf.Services[name] = cs
	}

	// Volumes block (top-level): one entry per declared local volume.
	if len(app.Volumes) > 0 {
		cf.Volumes = map[string]*composeVolume{}
		for _, v := range app.Volumes {
			cf.Volumes[v] = &composeVolume{Driver: "local"}
		}
	}

	// Networks block: always emit the private overlay; conditionally emit
	// the shared externals.
	cf.Networks = map[string]*composeNetwork{
		privateNet: {Driver: "overlay"},
	}
	if usesTraefikNet {
		cf.Networks[traefikNet] = &composeNetwork{External: true}
	}
	if usesMonitoringNet {
		cf.Networks[monitoringNet] = &composeNetwork{External: true}
	}

	// Secrets block: every secret declared at the App level becomes external.
	if len(app.Secrets) > 0 {
		cf.Secrets = map[string]*composeSecret{}
		for _, s := range app.Secrets {
			cf.Secrets[s] = &composeSecret{External: true}
		}
	}

	out, err := yaml.Marshal(cf)
	if err != nil {
		return nil, fmt.Errorf("marshal compose: %w", err)
	}
	return out, nil
}

// translateService renders one DSL service to its compose form. Updates
// usesTraefikNet / usesMonitoringNet via pointer so the top-level
// networks block matches what services actually reference.
func translateService(
	app *dsl.App,
	name string,
	s *dsl.Service,
	privateNet string,
	usesTraefikNet, usesMonitoringNet *bool,
) *composeService {
	cs := &composeService{
		Image:       s.Image,
		Command:     s.Command,
		Entrypoint:  s.Entrypoint,
		Environment: s.Env,
		Volumes:     s.Volumes,
		Secrets:     s.Secrets,
	}

	// Networks: every service is on the private overlay. Services with
	// `expose:` ALSO join traefik-net (so Traefik can reach them) and
	// monitoring-net (so OTel can scrape them).
	cs.Networks = []string{privateNet}
	if s.Expose != nil {
		cs.Networks = append(cs.Networks, traefikNet, monitoringNet)
		*usesTraefikNet = true
		*usesMonitoringNet = true
	}

	cs.Healthcheck = translateHealthcheck(s)

	cs.Deploy = translateDeploy(app, name, s)

	return cs
}

// translateHealthcheck handles both the shorthand types and the full-form.
// Shorthand maps to a CMD-SHELL invocation; full-form passes through.
func translateHealthcheck(s *dsl.Service) *composeHealthcheck {
	if s.Healthcheck == nil {
		return nil
	}
	h := s.Healthcheck

	// Shorthand → canonical compose healthcheck.
	switch h.Type {
	case "pg_isready":
		// Note the doubled $$: Docker Compose interpolates ${VAR} at deploy
		// time; we want POSTGRES_USER/POSTGRES_DB resolved at *runtime*
		// inside the container, so we escape with $$.
		return &composeHealthcheck{
			Test:     []string{"CMD-SHELL", "pg_isready -U $$POSTGRES_USER -d $$POSTGRES_DB"},
			Interval: "10s",
			Timeout:  "5s",
			Retries:  5,
		}
	case "http":
		port := 0
		if s.Expose != nil {
			port = s.Expose.Port
		}
		path := h.Path
		if path == "" {
			path = "/"
		}
		// wget is in most images; busybox/alpine ship it. We use --spider so
		// the body isn't downloaded.
		test := fmt.Sprintf("wget -q --spider http://localhost:%d%s", port, path)
		return &composeHealthcheck{
			Test:     []string{"CMD-SHELL", test},
			Interval: "10s",
			Timeout:  "5s",
			Retries:  5,
		}
	}

	// Full-form passthrough.
	return &composeHealthcheck{
		Test:     h.Test,
		Interval: h.Interval,
		Timeout:  h.Timeout,
		Retries:  h.Retries,
	}
}

func translateDeploy(app *dsl.App, name string, s *dsl.Service) *composeDeploy {
	d := &composeDeploy{
		Labels: standardLabels(app, name),
	}

	// Replicas / mode / restart policy.
	switch {
	case s.RunOnce:
		d.RestartPolicy = &composeRestartPolicy{Condition: "none"}
	default:
		replicas := 1
		if s.Replicas != nil {
			replicas = *s.Replicas
		}
		d.Replicas = &replicas
		d.RestartPolicy = &composeRestartPolicy{Condition: "on-failure"}
	}

	// Placement.
	switch s.Placement {
	case "manager":
		d.Placement = &composePlacement{Constraints: []string{"node.role == manager"}}
	case "worker":
		d.Placement = &composePlacement{Constraints: []string{"node.role == worker"}}
	}

	// Update policy: defaults applied even if Update is nil, so re-deploys
	// roll cleanly. Skip for run-once jobs — they have no update lifecycle.
	if !s.RunOnce {
		d.UpdateConfig = translateUpdate(s.Update)
	}

	// Traefik labels for exposed services.
	if s.Expose != nil {
		addTraefikLabels(d.Labels, app, name, s.Expose)
	}

	return d
}

// standardLabels are stamped on every service so operators (and Komodo /
// Portainer / future UIs) can group services by app/env/version.
func standardLabels(app *dsl.App, serviceName string) map[string]string {
	return map[string]string{
		labelService:     serviceName,
		labelApplication: app.Name,
		labelEnvironment: app.Env,
		labelVersion:     app.Version,
	}
}

// addTraefikLabels mutates the labels map in place with the standard
// Traefik routing config for an exposed service. Router/service names are
// scoped as <app>-<service> to avoid collisions across apps.
func addTraefikLabels(labels map[string]string, app *dsl.App, serviceName string, exp *dsl.Expose) {
	scope := app.Name + "-" + serviceName
	labels["traefik.enable"] = "true"
	labels["traefik.http.routers."+scope+".rule"] = "Host(`" + exp.Host + "`)"
	labels["traefik.http.routers."+scope+".entrypoints"] = "websecure"
	labels["traefik.http.routers."+scope+".tls"] = "true"
	labels["traefik.http.services."+scope+".loadbalancer.server.port"] = fmt.Sprintf("%d", exp.Port)
	labels["traefik.docker.network"] = traefikNet
}

func translateUpdate(u *dsl.Update) *composeUpdateConfig {
	out := &composeUpdateConfig{
		Parallelism: 1,
		Delay:       "10s",
		Order:       "start-first",
	}
	if u != nil {
		if u.Parallelism != 0 {
			out.Parallelism = u.Parallelism
		}
		if u.Delay != "" {
			out.Delay = u.Delay
		}
		if u.Order != "" {
			out.Order = u.Order
		}
	}
	return out
}

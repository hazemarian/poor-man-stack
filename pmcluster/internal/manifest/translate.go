package manifest

import (
	"fmt"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-stack/pmcluster/pkg/dsl"
)

// Shared external networks ensured by `pmcluster cluster up`; the
// translator references them with `external: true`.
const (
	traefikNet    = "traefik-net"
	monitoringNet = "monitoring-net"
)

// privateNetSuffix forms the per-app inline overlay (e.g.
// "donation-campaign-net").
const privateNetSuffix = "-net"

const (
	labelService     = "service"
	labelApplication = "application"
	labelEnvironment = "environment"
	labelVersion     = "version"
)

// Translate renders a validated, interpolated *dsl.App into compose v3.9
// YAML. CALLER MUST run Parse → Interpolate → Validate first; Translate
// does no validation itself.
func Translate(app *dsl.App) ([]byte, error) {
	cf := &composeFile{
		Version:  "3.9",
		Services: map[string]*composeService{},
	}

	privateNet := app.Name + privateNetSuffix

	// Only emit shared networks at the top level if at least one service
	// references them.
	usesTraefikNet := false
	usesMonitoringNet := false

	for name, svc := range app.Services {
		cs := translateService(app, name, svc, privateNet, &usesTraefikNet, &usesMonitoringNet)
		cf.Services[name] = cs
	}

	if len(app.Volumes) > 0 {
		cf.Volumes = map[string]*composeVolume{}
		for _, v := range app.Volumes {
			cf.Volumes[v] = &composeVolume{Driver: "local"}
		}
	}

	cf.Networks = map[string]*composeNetwork{
		privateNet: {Driver: "overlay"},
	}
	if usesTraefikNet {
		cf.Networks[traefikNet] = &composeNetwork{External: true}
	}
	if usesMonitoringNet {
		cf.Networks[monitoringNet] = &composeNetwork{External: true}
	}

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

// translateService updates the uses*Net pointers so the top-level
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

	// Exposed services join traefik-net (so Traefik can reach them) and
	// monitoring-net (so OTel can scrape them) on top of the private overlay.
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

func translateHealthcheck(s *dsl.Service) *composeHealthcheck {
	if s.Healthcheck == nil {
		return nil
	}
	h := s.Healthcheck

	switch h.Type {
	case "pg_isready":
		// $$ escapes Compose's deploy-time ${VAR} interpolation so
		// POSTGRES_USER/POSTGRES_DB resolve at runtime inside the container.
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
		test := fmt.Sprintf("wget -q --spider http://localhost:%d%s", port, path)
		return &composeHealthcheck{
			Test:     []string{"CMD-SHELL", test},
			Interval: "10s",
			Timeout:  "5s",
			Retries:  5,
		}
	}

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

	switch s.Placement {
	case "manager":
		d.Placement = &composePlacement{Constraints: []string{"node.role == manager"}}
	case "worker":
		d.Placement = &composePlacement{Constraints: []string{"node.role == worker"}}
	}

	// Defaults apply even when Update is nil so re-deploys roll cleanly;
	// run-once jobs have no update lifecycle.
	if !s.RunOnce {
		d.UpdateConfig = translateUpdate(s.Update)
	}

	if s.Expose != nil {
		addTraefikLabels(d.Labels, app, name, s.Expose)
	}

	return d
}

func standardLabels(app *dsl.App, serviceName string) map[string]string {
	return map[string]string{
		labelService:     serviceName,
		labelApplication: app.Name,
		labelEnvironment: app.Env,
		labelVersion:     app.Version,
	}
}

// addTraefikLabels scopes router/service names as <app>-<service> to
// avoid collisions across apps.
//
// The cluster-wide cors-default middleware (defined in the Traefik
// dynamic file provider by pmcluster cluster up) is attached by
// default. Services that own CORS themselves opt out via
// expose.cors_disabled: true.
func addTraefikLabels(labels map[string]string, app *dsl.App, serviceName string, exp *dsl.Expose) {
	scope := app.Name + "-" + serviceName
	labels["traefik.enable"] = "true"
	labels["traefik.http.routers."+scope+".rule"] = "Host(`" + exp.Host + "`)"
	labels["traefik.http.routers."+scope+".entrypoints"] = "websecure"
	labels["traefik.http.routers."+scope+".tls"] = "true"
	if !exp.CORSDisabled {
		labels["traefik.http.routers."+scope+".middlewares"] = "cors-default@file"
	}
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

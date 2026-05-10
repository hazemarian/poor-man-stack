package cluster

import (
	"embed"
	"encoding/base64"
	"fmt"
	"io/fs"
	"strings"
)

// embeddedStacks holds the source-of-truth copies of the bundled compose
// files. The on-disk repo also has these under stacks/ for human readability,
// but pmcluster always uses the embedded copies — edits to stacks/*.yml
// after `cluster up` have no effect.
//
//go:embed embeds
var embeddedStacks embed.FS

// embeddedDir is the directory inside embeddedStacks. Symbolic so the embed
// directive and the lookup stay in sync.
const embeddedDir = "embeds"

// stackName uniquely identifies a bundled compose stack.
type stackName string

const (
	StackInfra         stackName = "infra"
	StackObservability stackName = "observability"
	StackBackup        stackName = "backup"
)

// composeFile maps each bundled stack to its embedded compose YAML.
var composeFile = map[stackName]string{
	StackInfra:         "infra-stack.yml",
	StackObservability: "observability-stack.yml",
	StackBackup:        "backup-stack.yml",
}

// RenderInput collects every operator-supplied value the templates and
// compose files need. Future flags get added here, not as separate args.
type RenderInput struct {
	Domain                   string
	OpenObserveAdminEmail    string
	OpenObserveAdminPassword string
}

// LoadComposeFile returns the rendered compose YAML for a bundled stack:
// the embedded literal file with `${DOMAIN}` and `${OPENOBSERVE_ADMIN_EMAIL}`
// substituted in.
//
// We do the substitution in Go (rather than letting `docker stack deploy`
// pick it up from the env) so pmcluster doesn't need an env-var contract
// with the docker CLI process.
func LoadComposeFile(name stackName, in RenderInput) ([]byte, error) {
	fname, ok := composeFile[name]
	if !ok {
		return nil, fmt.Errorf("unknown stack %q", name)
	}
	body, err := fs.ReadFile(embeddedStacks, embeddedDir+"/"+fname)
	if err != nil {
		return nil, fmt.Errorf("read embedded %s: %w", fname, err)
	}
	out := strings.ReplaceAll(string(body), "${DOMAIN}", in.Domain)
	out = strings.ReplaceAll(out, "${OPENOBSERVE_ADMIN_EMAIL}", in.OpenObserveAdminEmail)
	return []byte(out), nil
}

// otelCollectorConfigTemplate is the OTel Collector pipeline config,
// hardcoded so pmcluster ships it as a single binary. The single
// substitution point is the OpenObserve auth header value.
//
// Customisation: operators can override the entire body via
// `pmcluster cluster up --otel-config-file path/to/custom.yaml` (Phase 2.1.E)
// for unusual deployments; the hardcoded value is the supported default.
const otelCollectorConfigTemplate = `extensions:
  health_check:

  docker_observer:
    endpoint: "unix:///var/run/docker.sock"
    cache_sync_interval: 5s

receivers:
  otlp:
    protocols:
      grpc: { endpoint: "0.0.0.0:4317" }
      http: { endpoint: "0.0.0.0:4318" }

  docker_stats:
    endpoint: "unix:///var/run/docker.sock"
    collection_interval: 10s
    api_version: "1.44"
    metric_labels_to_resource_attributes:
      com.docker.swarm.service.name: "service.name"
      com.docker.stack.namespace:    "stack.name"
      com.docker.swarm.node.id:      "swarm.node.id"
      com.docker.swarm.task.name:    "container.name"

  receiver_creator:
    watch_observers: [docker_observer]
    receivers:
      filelog:
        rule: type == "container" and labels["io.otel.skip_file_logs"] != "true"
        config:
          include:
            - "/var/lib/docker/containers/` + "`" + `container_id` + "`" + `/` + "`" + `container_id` + "`" + `-json.log"
          exclude:
            - "/var/lib/docker/containers/*otel-collector*/*-json.log"
          include_file_name: false
          start_at: end
          storage: null
          attributes:
            container.name:       "` + "`" + `name` + "`" + `"
            container.id:         "` + "`" + `container_id` + "`" + `"
            service.name:         "` + "`" + `label:com.docker.swarm.service.name` + "`" + `"
            service.namespace:    "` + "`" + `label:com.docker.stack.namespace` + "`" + `"
            container.image.name: "` + "`" + `image` + "`" + `"
          operators:
            - type: json_parser
              timestamp:
                parse_from: attributes.time
                layout: '%Y-%m-%dT%H:%M:%S.%LZ'
              severity:
                parse_from: attributes.stream
                mapping: { info: stdout, error: stderr }
            - type: move
              from: attributes.log
              to: body
            - type: remove
              field: attributes.time
            - type: remove
              field: attributes.stream
            - type: recombine
              combine_field: body
              is_first_entry: body matches "^\\d{4}-\\d{2}-\\d{2}|^\\[?\\d{4}-\\d{2}-\\d{2}"
              source_identifier: attributes["container.id"]

processors:
  resource:
    attributes:
      - key: service.name
        action: upsert
        from_attribute: "container.labels.com.docker.swarm.service.name"
      - key: stack.name
        action: upsert
        from_attribute: "container.labels.com.docker.stack.namespace"

  resourcedetection:
    detectors: [system, docker]
    override: false

  transform:
    error_mode: ignore
    log_statements:
      - context: resource
        statements:
          - set(attributes["service.name"], attributes["container.name"]) where attributes["service.name"] == "" or attributes["service.name"] == nil
          - set(attributes["service.namespace"], "standalone") where attributes["service.namespace"] == "" or attributes["service.namespace"] == nil
      - context: log
        statements:
          # Noise filtering
          - drop() where IsMatch(body, "ELB-HealthChecker/2.0")
          - drop() where IsMatch(body, "kube-probe")
          - drop() where IsMatch(body, "GET /health HTTP") and attributes["stream"] == "stdout"
          # Severity from Docker stream
          - set(attributes["severity_text"], "INFO")  where attributes["stream"] == "stdout"
          - set(attributes["severity_number"], 9)     where attributes["stream"] == "stdout"
          - set(attributes["severity_text"], "ERROR") where attributes["stream"] == "stderr"
          - set(attributes["severity_number"], 17)    where attributes["stream"] == "stderr"
          # Severity defaults for OTLP-originated logs
          - set(attributes["severity_number"], 9)    where severity_number == 0
          - set(attributes["severity_text"], "INFO") where severity_number == 0
          # Severity escalation from log body keywords
          - set(attributes["severity_text"], "WARN") where severity_number <= 9  and IsMatch(body, "(?i)(\\bwarn\\b|\\bwarning\\b|\\[warn\\])")
          - set(attributes["severity_number"], 13)   where severity_number <= 9  and IsMatch(body, "(?i)(\\bwarn\\b|\\bwarning\\b|\\[warn\\])")
          - set(attributes["severity_text"], "ERROR") where severity_number <= 13 and IsMatch(body, "(?i)(\\berror\\b|\\berr\\b|\\bcritical\\b|\\[error\\])")
          - set(attributes["severity_number"], 17)    where severity_number <= 13 and IsMatch(body, "(?i)(\\berror\\b|\\berr\\b|\\bcritical\\b|\\[error\\])")
          # JSON log parsing and body promotion
          - merge_maps(attributes, ParseJSON(body), "upsert") where IsMatch(body, "^\\{")
          - set(attributes["severity_text"], attributes["level"]) where attributes["level"] != nil
          - set(body, attributes["msg"])     where attributes["msg"] != nil
          - set(body, attributes["message"]) where attributes["message"] != nil

  batch:
    send_batch_size: 1000
    timeout: 10s

exporters:
  otlp/openobserve:
    endpoint: "openobserve:5081"
    tls:
      insecure: true
    headers:
      Authorization: "__BASIC_AUTH_PLACEHOLDER__"
      organization: "default"

service:
  extensions: [health_check, docker_observer]
  telemetry:
    logs:
      level: warn
  pipelines:
    logs:
      receivers:  [receiver_creator, otlp]
      processors: [resource, transform, batch]
      exporters:  [otlp/openobserve]
    metrics:
      receivers:  [docker_stats, otlp]
      processors: [resourcedetection, resource, batch]
      exporters:  [otlp/openobserve]
    traces:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [otlp/openobserve]
`

// traefikDynamicTemplate is the Traefik file-provider config: TLS cert
// wiring, the admin-auth basicAuth middleware, and the pmcluster route
// (Host(...) → host.docker.internal:9090). The single substitution point
// is the operator's domain.
const traefikDynamicTemplate = `# Generated by pmcluster cluster up. Shipped as a Docker config so it's
# replicated to every Swarm node automatically.
http:
  middlewares:
    admin-auth:
      basicAuth:
        usersFile: /run/secrets/admin_credentials
  routers:
    pmcluster:
      rule: "Host(` + "`" + `pmcluster.__DOMAIN__` + "`" + `)"
      entrypoints: [websecure]
      service: pmcluster
      tls: {}
  services:
    pmcluster:
      loadBalancer:
        servers:
          - url: "http://host.docker.internal:9090"
tls:
  certificates:
    - certFile: /run/secrets/cert
      keyFile: /run/secrets/key
  stores:
    default:
      defaultCertificate:
        certFile: /run/secrets/cert
        keyFile: /run/secrets/key
`

// RenderOTelCollectorConfig substitutes __BASIC_AUTH_PLACEHOLDER__ in the
// hardcoded template with `Basic <b64(email:password)>` for OpenObserve.
//
// The OTel Collector posts to OpenObserve over OTLP/HTTP using HTTP Basic
// Auth — the placeholder is the full `Authorization: Basic <b64>` value.
func RenderOTelCollectorConfig(in RenderInput) ([]byte, error) {
	if in.OpenObserveAdminEmail == "" || in.OpenObserveAdminPassword == "" {
		return nil, fmt.Errorf("RenderOTelCollectorConfig: OpenObserve email and password are required")
	}
	cred := in.OpenObserveAdminEmail + ":" + in.OpenObserveAdminPassword
	basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(cred))
	rendered := strings.ReplaceAll(otelCollectorConfigTemplate, "__BASIC_AUTH_PLACEHOLDER__", basicAuth)
	return []byte(rendered), nil
}

// RenderTraefikDynamic substitutes __DOMAIN__ in the hardcoded template with
// the operator's domain.
func RenderTraefikDynamic(in RenderInput) ([]byte, error) {
	if in.Domain == "" {
		return nil, fmt.Errorf("RenderTraefikDynamic: Domain is required")
	}
	rendered := strings.ReplaceAll(traefikDynamicTemplate, "__DOMAIN__", in.Domain)
	return []byte(rendered), nil
}

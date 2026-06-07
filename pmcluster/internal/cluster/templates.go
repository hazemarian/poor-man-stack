package cluster

import (
	"bytes"
	"embed"
	"encoding/base64"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"text/template"
)

// embeddedStacks holds the source-of-truth bundled compose files.
//
//go:embed embeds
var embeddedStacks embed.FS

const embeddedDir = "embeds"

type stackName string

const (
	StackInfra         stackName = "infra"
	StackObservability stackName = "observability"
	StackBackup        stackName = "backup"
)

var composeFile = map[stackName]string{
	StackInfra:         "infra-stack.yml",
	StackObservability: "observability-stack.yml",
	StackBackup:        "backup-stack.yml",
}

type RenderInput struct {
	Domain                   string
	OpenObserveAdminEmail    string
	OpenObserveAdminPassword string

	// ACMEEmail enables Let's Encrypt automation when non-empty. Mutually
	// exclusive with operator-supplied cert/key (see cluster.UpInput).
	ACMEEmail string

	// CORSOriginRegex is the Traefik accessControlAllowOriginListRegex
	// pattern wired into the cors-default middleware. When empty,
	// RenderTraefikDynamic derives it from Domain via CORSOriginRegex().
	CORSOriginRegex string
}

// LoadComposeFile renders an embedded stack: text/template first (for
// {{if .ACMEEmail}} blocks), then ${DOMAIN}/${OPENOBSERVE_ADMIN_EMAIL}
// substitution. The split lets simple stacks (observability, backup)
// stay free of template directives.
func LoadComposeFile(name stackName, in RenderInput) ([]byte, error) {
	fname, ok := composeFile[name]
	if !ok {
		return nil, fmt.Errorf("unknown stack %q", name)
	}
	body, err := fs.ReadFile(embeddedStacks, embeddedDir+"/"+fname)
	if err != nil {
		return nil, fmt.Errorf("read embedded %s: %w", fname, err)
	}
	// Custom delims so offen's runtime `{{ .Node.ID }}` template syntax
	// in backup-stack.yml is left untouched.
	tmpl, err := template.New(string(name)).Delims("[[", "]]").Parse(string(body))
	if err != nil {
		return nil, fmt.Errorf("parse %s template: %w", fname, err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, in); err != nil {
		return nil, fmt.Errorf("execute %s template: %w", fname, err)
	}
	out := strings.ReplaceAll(rendered.String(), "${DOMAIN}", in.Domain)
	out = strings.ReplaceAll(out, "${OPENOBSERVE_ADMIN_EMAIL}", in.OpenObserveAdminEmail)
	return []byte(out), nil
}

// otelCollectorConfigTemplate is the OTel Collector pipeline. The single
// substitution point is __BASIC_AUTH_PLACEHOLDER__ (OpenObserve auth).
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
            - /var/lib/docker/containers/__BT__container_id__BT__/__BT__container_id__BT__-json.log
          exclude:
            - /var/lib/docker/containers/*otel-collector*/*-json.log
          include_file_name: false
          start_at: end
          storage: null
          attributes:
            container.name:       __BT__name__BT__
            container.id:         __BT__container_id__BT__
            service.name:         __BT__label:__QT__com.docker.swarm.service.name__QT____BT__
            service.namespace:    __BT__label:__QT__com.docker.stack.namespace__QT____BT__
            container.image.name: __BT__image__BT__
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

  filter/noise:
    error_mode: ignore
    logs:
      log_record:
        - 'not IsMatch(body, "ELB-HealthChecker/2.0")'
        - 'not IsMatch(body, "kube-probe")'
        - 'not (IsMatch(body, "GET /health HTTP") and attributes["stream"] == "stdout")'

  transform:
    error_mode: ignore
    log_statements:
      - context: resource
        statements:
          - set(attributes["service.name"], attributes["container.name"]) where attributes["service.name"] == "" or attributes["service.name"] == nil
          - set(attributes["service.namespace"], "standalone") where attributes["service.namespace"] == "" or attributes["service.namespace"] == nil
      - context: log
        statements:
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
      processors: [resource, filter/noise, transform, batch]
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

// traefikDynamicTemplate is a text/template — [[if .ACMEEmail]] switches
// the file-provider router between ACME and operator-supplied cert/key.
// Custom delims [[..]] match LoadComposeFile so embedded YAMLs are uniform.
const traefikDynamicTemplate = `# Generated by pmcluster cluster up. Shipped as a Docker config so it's
# replicated to every Swarm node automatically.
http:
  middlewares:
    admin-auth:
      basicAuth:
        usersFile: /run/secrets/admin_credentials
    # cors-default: applied to every managed router (pmcluster, traefik
    # dashboard, portainer, openobserve) AND every exposed app router
    # emitted by the manifest translator. Origins are restricted to
    # https://<any-subdomain>.<cluster-domain> + https://<cluster-domain>
    # via a regex derived from cluster up --domain. Echoed origins keep
    # accessControlAllowCredentials valid (spec forbids "*" + credentials).
    cors-default:
      headers:
        accessControlAllowOriginListRegex:
          - "[[.CORSOriginRegex]]"
        accessControlAllowMethods:
          - GET
          - POST
          - PUT
          - PATCH
          - DELETE
          - OPTIONS
        accessControlAllowHeaders:
          - Content-Type
          - Authorization
          - X-Pmcluster-Signature
          - X-Request-Id
        accessControlAllowCredentials: true
        accessControlMaxAge: 600
        addVaryHeader: true
  routers:
    pmcluster:
      rule: "Host(` + "`" + `pmcluster.[[.Domain]]` + "`" + `)"
      entrypoints: [websecure]
      service: pmcluster
      middlewares:
        - cors-default
      tls:
        [[- if .ACMEEmail]]
        certResolver: letsencrypt
        [[- end]]
  services:
    pmcluster:
      loadBalancer:
        servers:
          - url: "http://host.docker.internal:9090"
[[- if not .ACMEEmail]]
tls:
  certificates:
    - certFile: /run/secrets/cert
      keyFile: /run/secrets/key
  stores:
    default:
      defaultCertificate:
        certFile: /run/secrets/cert
        keyFile: /run/secrets/key
[[- end]]
`

// RenderOTelCollectorConfig fills in the OpenObserve `Authorization: Basic <b64>`
// header value.
func RenderOTelCollectorConfig(in RenderInput) ([]byte, error) {
	if in.OpenObserveAdminEmail == "" || in.OpenObserveAdminPassword == "" {
		return nil, fmt.Errorf("RenderOTelCollectorConfig: OpenObserve email and password are required")
	}
	cred := in.OpenObserveAdminEmail + ":" + in.OpenObserveAdminPassword
	basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(cred))
	rendered := strings.ReplaceAll(otelCollectorConfigTemplate, "__BASIC_AUTH_PLACEHOLDER__", basicAuth)
	// __BT__ → backtick, __QT__ → double-quote (for receiver_creator expressions)
	rendered = strings.ReplaceAll(rendered, "__BT__", "`")
	rendered = strings.ReplaceAll(rendered, "__QT__", "\"")
	return []byte(rendered), nil
}

func RenderTraefikDynamic(in RenderInput) ([]byte, error) {
	if in.Domain == "" {
		return nil, fmt.Errorf("RenderTraefikDynamic: Domain is required")
	}
	if in.CORSOriginRegex == "" {
		in.CORSOriginRegex = CORSOriginRegex(in.Domain)
	}
	tmpl, err := template.New("traefik-dynamic").Delims("[[", "]]").Parse(traefikDynamicTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse traefik dynamic template: %w", err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, in); err != nil {
		return nil, fmt.Errorf("execute traefik dynamic template: %w", err)
	}
	return out.Bytes(), nil
}

// validDomain matches a DNS host: dot-separated labels of letters,
// digits, and hyphens (no scheme, no port, no path). Used so callers
// can't smuggle regex metachars through Domain into the template.
var validDomain = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)+$`)

// CORSOriginRegex returns the Traefik origin-list regex that allows
// https://<domain> and https://<any-subdomain>.<domain>. Falls back to
// a never-match pattern when domain is empty or malformed so the
// middleware never inadvertently opens up "*".
func CORSOriginRegex(domain string) string {
	if !validDomain.MatchString(domain) {
		// Match nothing — callers should validate Domain upstream;
		// this keeps the template render-safe in the worst case.
		return `^$`
	}
	escaped := regexp.QuoteMeta(strings.ToLower(domain))
	// Each subdomain label must start with an alphanumeric (no leading
	// hyphen — RFC 1123) to avoid letting weird-but-not-quite-impossible
	// origins through.
	return `^https://([a-z0-9][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*)*\.)?` + escaped + `$`
}

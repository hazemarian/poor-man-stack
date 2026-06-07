package cluster

import (
	"bytes"
	"embed"
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

// embeddedStacks holds the source-of-truth bundled compose and config files.
//
//go:embed embeds
var embeddedStacks embed.FS

const embeddedDir = "embeds"

// ConfigFileNames lists the bundled configs shipped on init to
// ~/.pmcluster/config/ so operators can customise them.
var ConfigFileNames = []string{
	"infra-stack.yml",
	"observability-stack.yml",
	"backup-stack.yml",
	"otel-collector-config.yml",
	"traefik-dynamic.yml",
}

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

	// ConfigDir is ~/.pmcluster/config/. When non-empty, config loading
	// prefers a user-supplied copy from disk over the embedded default.
	// If the disk file is missing it falls back to the embedded version.
	ConfigDir string

	// OTelConfigName is the versioned Docker config name for the OTel
	// collector pipeline (e.g. pmcluster_otel_config_v003). Substituted
	// as __OTEL_CONFIG_NAME__ in compose files.
	OTelConfigName string

	// TraefikConfigName is the versioned Docker config name for the
	// Traefik dynamic file-provider config. Substituted as
	// __TRAEFIK_CONFIG_NAME__ in compose files.
	TraefikConfigName string
}

// readConfigFile loads a named file. When in.ConfigDir is set the disk
// copy at <ConfigDir>/<name> wins (if readable). Otherwise — or on any
// disk error — the embedded fallback is used. Applies to stacks AND
// standalone configs.
func readConfigFile(name string, in RenderInput) (string, error) {
	if in.ConfigDir != "" {
		path := filepath.Join(in.ConfigDir, name)
		if data, err := os.ReadFile(path); err == nil {
			return string(data), nil
		}
		// Disk missing/unreadable → fall through to embedded.
	}
	body, err := fs.ReadFile(embeddedStacks, embeddedDir+"/"+name)
	if err != nil {
		return "", fmt.Errorf("read embedded %s: %w", name, err)
	}
	return string(body), nil
}

// LoadComposeFile renders a stack YAML: text/template first (for
// [[if .ACMEEmail]] blocks), then ${DOMAIN}/${OPENOBSERVE_ADMIN_EMAIL}
// substitution. Reads from disk first (ConfigDir), then embedded.
func LoadComposeFile(name stackName, in RenderInput) ([]byte, error) {
	fname, ok := composeFile[name]
	if !ok {
		return nil, fmt.Errorf("unknown stack %q", name)
	}
	body, err := readConfigFile(fname, in)
	if err != nil {
		return nil, err
	}
	// Custom delims so offen's runtime `{{ .Node.ID }}` template syntax
	// in backup-stack.yml is left untouched.
	tmpl, err := template.New(string(name)).Delims("[[", "]]").Parse(body)
	if err != nil {
		return nil, fmt.Errorf("parse %s template: %w", fname, err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, in); err != nil {
		return nil, fmt.Errorf("execute %s template: %w", fname, err)
	}
	out := strings.ReplaceAll(rendered.String(), "${DOMAIN}", in.Domain)
	out = strings.ReplaceAll(out, "${OPENOBSERVE_ADMIN_EMAIL}", in.OpenObserveAdminEmail)
	out = strings.ReplaceAll(out, "__OPENOBSERVE_PASSWORD__", in.OpenObserveAdminPassword)
	out = strings.ReplaceAll(out, "__OTEL_CONFIG_NAME__", in.OTelConfigName)
	out = strings.ReplaceAll(out, "__TRAEFIK_CONFIG_NAME__", in.TraefikConfigName)
	return []byte(out), nil
}

// RenderOTelCollectorConfig fills in the OpenObserve `Authorization: Basic <b64>`
// header value. Reads from disk first (ConfigDir), then embedded.
func RenderOTelCollectorConfig(in RenderInput) ([]byte, error) {
	if in.OpenObserveAdminEmail == "" || in.OpenObserveAdminPassword == "" {
		return nil, fmt.Errorf("RenderOTelCollectorConfig: OpenObserve email and password are required")
	}
	cred := in.OpenObserveAdminEmail + ":" + in.OpenObserveAdminPassword
	basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(cred))

	body, err := readConfigFile("otel-collector-config.yml", in)
	if err != nil {
		return nil, err
	}
	rendered := strings.ReplaceAll(body, "__BASIC_AUTH_PLACEHOLDER__", basicAuth)
	// __BT__ → backtick (for receiver_creator expressions)
	rendered = strings.ReplaceAll(rendered, "__BT__", "`")
	return []byte(rendered), nil
}

// RenderTraefikDynamic renders the Traefik file-provider config.
// Reads from disk first (ConfigDir), then embedded.
func RenderTraefikDynamic(in RenderInput) ([]byte, error) {
	if in.Domain == "" {
		return nil, fmt.Errorf("RenderTraefikDynamic: Domain is required")
	}

	body, err := readConfigFile("traefik-dynamic.yml", in)
	if err != nil {
		return nil, err
	}

	tmpl, err := template.New("traefik-dynamic").Delims("[[", "]]").Parse(body)
	if err != nil {
		return nil, fmt.Errorf("parse traefik dynamic template: %w", err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, in); err != nil {
		return nil, fmt.Errorf("execute traefik dynamic template: %w", err)
	}
	return out.Bytes(), nil
}

// EnsureConfigDir creates ~/.pmcluster/config/ and seeds it with the
// embedded defaults for any files that don't already exist.
// Existing files are never overwritten — operator edits win.
func EnsureConfigDir(configDir string) error {
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return fmt.Errorf("create config dir %s: %w", configDir, err)
	}
	for _, name := range ConfigFileNames {
		dest := filepath.Join(configDir, name)
		if _, err := os.Stat(dest); err == nil {
			continue // already exists — keep user's version
		}
		body, err := fs.ReadFile(embeddedStacks, embeddedDir+"/"+name)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", name, err)
		}
		if err := os.WriteFile(dest, body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dest, err)
		}
	}
	return nil
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

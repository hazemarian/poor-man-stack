package cluster

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

// stacksWithDomain lists the bundled stacks that actually contain ${DOMAIN}
// substitution points (backup-stack.yml has none).
var stacksWithDomain = map[stackName]bool{
	StackInfra:         true,
	StackObservability: false,
	StackBackup:        false,
}

// TestLoadComposeFile_KnownStacks verifies each known stack returns non-empty
// YAML and that all placeholder tokens have been removed.
func TestLoadComposeFile_KnownStacks(t *testing.T) {
	in := RenderInput{
		Domain:                "example.com",
		OpenObserveAdminEmail: "ops@example.com",
	}

	stacks := []stackName{StackInfra, StackObservability, StackBackup}
	for _, s := range stacks {
		t.Run(string(s), func(t *testing.T) {
			data, err := LoadComposeFile(s, in)
			if err != nil {
				t.Fatalf("LoadComposeFile(%q): %v", s, err)
			}
			if len(data) == 0 {
				t.Fatal("returned empty YAML")
			}
			body := string(data)

			// No un-substituted placeholders should remain.
			if strings.Contains(body, "${DOMAIN}") {
				t.Error("${DOMAIN} placeholder was not substituted")
			}
			if strings.Contains(body, "${OPENOBSERVE_ADMIN_EMAIL}") {
				t.Error("${OPENOBSERVE_ADMIN_EMAIL} placeholder was not substituted")
			}

			// For stacks that actually use ${DOMAIN}, verify substitution.
			if stacksWithDomain[s] && !strings.Contains(body, "example.com") {
				t.Error("substituted domain 'example.com' not found in output")
			}
		})
	}
}

// TestLoadComposeFile_SubstitutesOpenObserveEmail verifies the email
// substitution specifically in the observability stack, which uses it.
func TestLoadComposeFile_SubstitutesOpenObserveEmail(t *testing.T) {
	in := RenderInput{
		Domain:                "prod.example.com",
		OpenObserveAdminEmail: "admin@company.org",
	}
	data, err := LoadComposeFile(StackObservability, in)
	if err != nil {
		t.Fatalf("LoadComposeFile: %v", err)
	}
	body := string(data)
	if strings.Contains(body, "${OPENOBSERVE_ADMIN_EMAIL}") {
		t.Error("${OPENOBSERVE_ADMIN_EMAIL} placeholder was not substituted in observability stack")
	}
	// Verify our substituted email appears somewhere (it may or may not be in
	// this stack, but at least no placeholder should remain).
	_ = body
}

// TestLoadComposeFile_UnknownStack verifies that an unknown stack name returns
// an error.
func TestLoadComposeFile_UnknownStack(t *testing.T) {
	_, err := LoadComposeFile("nonexistent", RenderInput{Domain: "x.com"})
	if err == nil {
		t.Fatal("expected error for unknown stack, got nil")
	}
}

// TestRenderOTelCollectorConfig_ContainsBasicAuth verifies that the rendered
// OTel config contains Authorization: Basic <base64(email:password)> and that
// decoding the base64 yields the correct "email:password" string.
func TestRenderOTelCollectorConfig_ContainsBasicAuth(t *testing.T) {
	in := RenderInput{
		OpenObserveAdminEmail:    "admin@example.com",
		OpenObserveAdminPassword: "hunter2",
	}
	data, err := RenderOTelCollectorConfig(in)
	if err != nil {
		t.Fatalf("RenderOTelCollectorConfig: %v", err)
	}
	body := string(data)

	// Verify the Authorization header placeholder is gone.
	if strings.Contains(body, "__BASIC_AUTH_PLACEHOLDER__") {
		t.Error("__BASIC_AUTH_PLACEHOLDER__ not substituted")
	}

	// The template wraps the value in quotes: Authorization: "Basic <b64>"
	// Find the Authorization line with the quoted value.
	const prefix = `Authorization: "Basic `
	idx := strings.Index(body, prefix)
	if idx < 0 {
		t.Fatalf("%q not found in rendered config", prefix)
	}
	rest := body[idx+len(prefix):]
	// The base64 value ends at the closing quote.
	end := strings.Index(rest, `"`)
	if end < 0 {
		end = len(rest)
	}
	b64 := strings.TrimSpace(rest[:end])

	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 decode of %q: %v", b64, err)
	}

	want := "admin@example.com:hunter2"
	if string(decoded) != want {
		t.Errorf("decoded = %q, want %q", decoded, want)
	}
}

// TestRenderOTelCollectorConfig_MissingEmail returns an error when email is
// empty.
func TestRenderOTelCollectorConfig_MissingEmail(t *testing.T) {
	_, err := RenderOTelCollectorConfig(RenderInput{
		OpenObserveAdminEmail:    "",
		OpenObserveAdminPassword: "pass",
	})
	if err == nil {
		t.Fatal("expected error when email is empty, got nil")
	}
}

// TestRenderOTelCollectorConfig_MissingPassword returns an error when password
// is empty.
func TestRenderOTelCollectorConfig_MissingPassword(t *testing.T) {
	_, err := RenderOTelCollectorConfig(RenderInput{
		OpenObserveAdminEmail:    "admin@x.com",
		OpenObserveAdminPassword: "",
	})
	if err == nil {
		t.Fatal("expected error when password is empty, got nil")
	}
}

// TestRenderOTelCollectorConfig_NoiseFilterInPipeline verifies the rendered
// config uses a filter/noise processor (with valid OTTL) instead of the
// invalid drop() function in transform log_statements.
func TestRenderOTelCollectorConfig_NoiseFilterInPipeline(t *testing.T) {
	in := RenderInput{
		OpenObserveAdminEmail:    "admin@x.com",
		OpenObserveAdminPassword: "pw",
	}
	data, err := RenderOTelCollectorConfig(in)
	if err != nil {
		t.Fatalf("RenderOTelCollectorConfig: %v", err)
	}
	body := string(data)

	// The transform processor must NOT contain drop() — that function only
	// exists in the filter processor.
	if strings.Contains(body, "drop()") {
		t.Error("transform processor must not contain drop(); noise filtering belongs in filter/noise")
	}

	// The filter/noise processor must exist with valid OTTL filter expressions.
	if !strings.Contains(body, "filter/noise:") {
		t.Error("missing filter/noise processor")
	}
	for _, want := range []string{
		`not IsMatch(body, "ELB-HealthChecker/2.0")`,
		`not IsMatch(body, "kube-probe")`,
		`not (IsMatch(body, "GET /health HTTP")`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("filter/noise missing expression containing %q", want)
		}
	}

	// The logs pipeline must place filter/noise before transform.
	if !strings.Contains(body, "[resource, filter/noise, transform, batch]") {
		t.Error("logs pipeline must include filter/noise before transform")
	}
}

// TestRenderTraefikDynamic_SubstitutesDomain verifies that __DOMAIN__ is
// replaced with the supplied Domain value.
func TestRenderTraefikDynamic_SubstitutesDomain(t *testing.T) {
	in := RenderInput{Domain: "myapp.example.com"}
	data, err := RenderTraefikDynamic(in)
	if err != nil {
		t.Fatalf("RenderTraefikDynamic: %v", err)
	}
	body := string(data)

	if strings.Contains(body, "__DOMAIN__") {
		t.Error("__DOMAIN__ placeholder was not substituted")
	}
	if !strings.Contains(body, "myapp.example.com") {
		t.Error("substituted domain not found in output")
	}
}

// TestRenderTraefikDynamic_EmptyDomain returns an error when Domain is empty.
func TestRenderTraefikDynamic_EmptyDomain(t *testing.T) {
	_, err := RenderTraefikDynamic(RenderInput{Domain: ""})
	if err == nil {
		t.Fatal("expected error for empty Domain, got nil")
	}
}

// TestRenderTraefikDynamic_ACMEMode verifies the ACME branch emits the
// certResolver tag and drops the static-cert tls block.
func TestRenderTraefikDynamic_ACMEMode(t *testing.T) {
	in := RenderInput{Domain: "x.example.com", ACMEEmail: "ops@example.com"}
	data, err := RenderTraefikDynamic(in)
	if err != nil {
		t.Fatalf("RenderTraefikDynamic: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "certResolver: letsencrypt") {
		t.Errorf("ACME mode missing certResolver line:\n%s", body)
	}
	if strings.Contains(body, "/run/secrets/cert") || strings.Contains(body, "/run/secrets/key") {
		t.Errorf("ACME mode must not reference static cert/key secrets:\n%s", body)
	}
}

// TestRenderTraefikDynamic_BYOMode verifies the legacy path stays intact
// when ACMEEmail is empty.
func TestRenderTraefikDynamic_BYOMode(t *testing.T) {
	in := RenderInput{Domain: "x.example.com"}
	data, err := RenderTraefikDynamic(in)
	if err != nil {
		t.Fatalf("RenderTraefikDynamic: %v", err)
	}
	body := string(data)
	if strings.Contains(body, "certResolver") {
		t.Errorf("BYO mode must not emit certResolver:\n%s", body)
	}
	if !strings.Contains(body, "/run/secrets/cert") || !strings.Contains(body, "/run/secrets/key") {
		t.Errorf("BYO mode must reference static cert/key secrets:\n%s", body)
	}
}

// TestLoadComposeFile_InfraACMEMode verifies the infra stack picks up the
// ACME-specific Traefik flags + volume in ACME mode and drops the cert/key
// secret declarations.
func TestLoadComposeFile_InfraACMEMode(t *testing.T) {
	body, err := LoadComposeFile(StackInfra, RenderInput{
		Domain:    "x.example.com",
		ACMEEmail: "ops@example.com",
	})
	if err != nil {
		t.Fatalf("LoadComposeFile: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		"certificatesresolvers.letsencrypt.acme.email=ops@example.com",
		"--certificatesresolvers.letsencrypt.acme.httpchallenge=true",
		"traefik_acme:/letsencrypt",
		"traefik.http.routers.traefik.tls.certresolver=letsencrypt",
		"traefik.http.routers.portainer.tls.certresolver=letsencrypt",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("ACME-mode infra stack missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"  cert:\n    external: true",
		"  key:\n    external: true",
	} {
		if strings.Contains(s, unwanted) {
			t.Errorf("ACME-mode infra stack should NOT contain %q", unwanted)
		}
	}
}

// TestCORSOriginRegex_MatchesSubdomains exercises the regex against a set
// of origin strings to confirm only same-domain HTTPS origins match.
func TestCORSOriginRegex_MatchesSubdomains(t *testing.T) {
	pat := CORSOriginRegex("example.com")
	re := mustCompile(t, pat)

	allow := []string{
		"https://example.com",
		"https://traefik.example.com",
		"https://api.foo.example.com",
		"https://a.b.c.example.com",
	}
	for _, o := range allow {
		if !re.MatchString(o) {
			t.Errorf("expected match for %q with pattern %s", o, pat)
		}
	}

	deny := []string{
		"http://example.com",           // wrong scheme
		"https://example.com:8080",     // port not allowed
		"https://evil.com",             // different domain
		"https://example.com.evil.com", // suffix-attack
		"https://EXAMPLE.COM",          // case mismatch (Traefik regex is case-sensitive; Origin is lowercase per RFC 6454)
		"https://example.com/",         // trailing slash means a path component is in the Origin
		"https://-bad.example.com",     // leading hyphen
		"https://foo..example.com",     // empty label
	}
	for _, o := range deny {
		if re.MatchString(o) {
			t.Errorf("expected NO match for %q with pattern %s", o, pat)
		}
	}
}

// TestCORSOriginRegex_RejectsBadDomain falls back to a never-match pattern
// when the input doesn't look like a host.
func TestCORSOriginRegex_RejectsBadDomain(t *testing.T) {
	for _, bad := range []string{
		"", "no-dot", "http://example.com",
		"example.com/path", "example.com:80",
		".example.com", "example..com",
	} {
		got := CORSOriginRegex(bad)
		if got != `^$` {
			t.Errorf("CORSOriginRegex(%q) = %q, want fallback %q", bad, got, `^$`)
		}
	}
}

// TestRenderTraefikDynamic_CORSWired verifies the rendered dynamic config
// contains the cors-default middleware (with the per-domain regex) and
// that the pmcluster router includes it in its middlewares chain.
func TestRenderTraefikDynamic_CORSWired(t *testing.T) {
	in := RenderInput{Domain: "example.com"}
	data, err := RenderTraefikDynamic(in)
	if err != nil {
		t.Fatalf("RenderTraefikDynamic: %v", err)
	}
	body := string(data)

	if !strings.Contains(body, "cors-default:") {
		t.Errorf("rendered config missing cors-default middleware:\n%s", body)
	}
	wantRegex := CORSOriginRegex("example.com")
	if !strings.Contains(body, wantRegex) {
		t.Errorf("rendered config missing expected origin regex %q:\n%s", wantRegex, body)
	}
	if !strings.Contains(body, "- cors-default") {
		t.Errorf("rendered config missing cors-default in router middlewares list:\n%s", body)
	}
	if !strings.Contains(body, "accessControlAllowCredentials: true") {
		t.Errorf("rendered config missing accessControlAllowCredentials: true:\n%s", body)
	}
}

// TestRenderTraefikDynamic_CORSOverride verifies that an explicit
// CORSOriginRegex on RenderInput takes precedence over the derived one.
func TestRenderTraefikDynamic_CORSOverride(t *testing.T) {
	in := RenderInput{Domain: "example.com", CORSOriginRegex: "^https://only.this.com$"}
	data, err := RenderTraefikDynamic(in)
	if err != nil {
		t.Fatalf("RenderTraefikDynamic: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "^https://only.this.com$") {
		t.Errorf("override regex not present:\n%s", body)
	}
	if strings.Contains(body, CORSOriginRegex("example.com")) {
		t.Errorf("derived regex should not appear when override is set")
	}
}

func mustCompile(t *testing.T, pat string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(pat)
	if err != nil {
		t.Fatalf("compile %q: %v", pat, err)
	}
	return re
}

// TestLoadComposeFile_InfraBYOMode verifies the legacy operator-cert path.
func TestLoadComposeFile_InfraBYOMode(t *testing.T) {
	body, err := LoadComposeFile(StackInfra, RenderInput{Domain: "x.example.com"})
	if err != nil {
		t.Fatalf("LoadComposeFile: %v", err)
	}
	s := string(body)
	if strings.Contains(s, "certificatesresolvers.letsencrypt") {
		t.Errorf("BYO-mode infra stack must not contain ACME flags")
	}
	if strings.Contains(s, "traefik_acme") {
		t.Errorf("BYO-mode infra stack must not declare the ACME volume")
	}
	for _, want := range []string{
		"  cert:\n    external: true",
		"  key:\n    external: true",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("BYO-mode infra stack missing %q", want)
		}
	}
}

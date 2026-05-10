package cluster

import (
	"context"
	"fmt"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// CredentialKind tags each managed credential with its consumer. The kind
// drives how the password gets serialised into the Swarm secret (plain vs
// htpasswd) and which service consumes it.
type CredentialKind string

const (
	KindTraefikAdmin CredentialKind = "traefik"
	KindPortainer    CredentialKind = "portainer"
	KindOpenObserve  CredentialKind = "openobserve"
)

// ManagedCredential is the in-memory shape of a bootstrap credential after
// either creation or load-from-store. Password is always plaintext here;
// callers may forward it to the operator (newly created) or to the renderer
// (e.g. the OTel basic-auth header).
//
// NewlyCreated tracks the pmcluster DB state ("we just inserted this row").
// SwarmSecretCreated tracks the Swarm state ("we just created this secret").
// In the normal case they match; they diverge in the "lost DB, kept Swarm"
// recovery scenario (DB is fresh, Swarm secret already existed) — see the
// known-issues note in the refactor plan.
type ManagedCredential struct {
	Name               string
	Kind               CredentialKind
	Username           string
	Password           string
	SwarmSecretName    string
	NewlyCreated       bool // pmcluster DB row was inserted this run
	SwarmSecretCreated bool // Swarm secret was created this run
}

// BootstrapInput is the operator-supplied data the credential bootstrap
// needs. Sourced from CLI flags + persisted config.
type BootstrapInput struct {
	TraefikAdminUser      string // typically "admin"
	OpenObserveAdminEmail string // operator's email — becomes OpenObserve admin username
}

// CredentialsManager bundles the dependencies the bootstrap + rotate flows need.
type CredentialsManager struct {
	Store    *store.Store
	Cipher   *credentials.Cipher
	Docker   docker.Client
	Deployer StackDeployer // optional — only required for Rotate's force-restart step
}

// consumingService maps each managed credential to the swarm service that
// mounts its secret. Used by Rotate to force-restart the service after
// the secret has been re-created with new content.
//
// Service names are "<stack>_<service>" because that's what `docker stack
// deploy` produces. Updating one of the bundled stacks should also update
// this map.
var consumingService = map[string]string{
	"traefik_dashboard": "infra_traefik",
	"portainer":         "infra_portainer",
	"openobserve_admin": "observability_openobserve",
}

// Bootstrap ensures every bundled component has a credential. Idempotent:
// re-running preserves existing values; only missing entries are created.
//
// On a fresh install, returns three NewlyCreated=true credentials with their
// plaintext passwords (operator should print these once and discard). On a
// re-run, returns the same set with NewlyCreated=false but plaintexts still
// populated (decrypted from store) so callers can render the OTel config.
//
// Specs (Phase 2):
//   - traefik_dashboard  : Traefik basicAuth admin (htpasswd format → admin_credentials)
//   - portainer          : Portainer admin password (raw → portainer_admin_password)
//   - openobserve_admin  : OpenObserve root password (raw → zo_root_user_password)
func (m *CredentialsManager) Bootstrap(ctx context.Context, in BootstrapInput) (map[string]*ManagedCredential, error) {
	if in.TraefikAdminUser == "" {
		in.TraefikAdminUser = "admin"
	}
	if in.OpenObserveAdminEmail == "" {
		return nil, fmt.Errorf("OpenObserve admin email is required (use --openobserve-email or set in config)")
	}

	// Take the canonical specs and fill in the per-credential username from
	// the bootstrap input.
	usernameFor := map[string]string{
		"traefik_dashboard": in.TraefikAdminUser,
		"portainer":         "admin",
		"openobserve_admin": in.OpenObserveAdminEmail,
	}
	specs := bootstrapSpecs()
	for i := range specs {
		specs[i].username = usernameFor[specs[i].name]
	}

	out := make(map[string]*ManagedCredential, len(specs))
	for _, spec := range specs {
		mc, err := m.ensure(ctx, spec)
		if err != nil {
			return nil, fmt.Errorf("bootstrap %s: %w", spec.name, err)
		}
		out[spec.name] = mc
	}
	return out, nil
}

// secretFormat decides how a credential's plaintext password is serialised
// into its Swarm secret payload.
type secretFormat int

const (
	formatPlain    secretFormat = iota // raw password bytes
	formatHtpasswd                     // "user:bcrypt(password)\n" line
)

// bootstrapSpec is the per-credential recipe consumed by ensure().
type bootstrapSpec struct {
	name            string
	kind            CredentialKind
	username        string
	swarmSecretName string
	format          secretFormat
}

// ensure implements the get-or-create logic for one credential.
//
//  1. Check the store. If found: decrypt password, ensure the matching
//     Swarm secret exists (re-create if missing), return NewlyCreated=false.
//  2. If missing: generate a random password, encrypt+insert in store,
//     create the Swarm secret with the appropriate payload format,
//     return NewlyCreated=true.
func (m *CredentialsManager) ensure(ctx context.Context, spec bootstrapSpec) (*ManagedCredential, error) {
	existing, err := m.Store.GetCredential(ctx, spec.name)
	if err == nil {
		// Existing credential — decrypt and ensure the Swarm side too.
		plaintext, err := m.Cipher.Decrypt(existing.PasswordCiphertext)
		if err != nil {
			return nil, fmt.Errorf("decrypt %s: %w", spec.name, err)
		}
		secretPayload, err := serialisePassword(spec, existing.Username, string(plaintext))
		if err != nil {
			return nil, err
		}
		secretCreated, err := EnsureSecret(ctx, m.Docker, existing.SwarmSecretName, secretPayload)
		if err != nil {
			return nil, fmt.Errorf("ensure swarm secret %s: %w", existing.SwarmSecretName, err)
		}
		return &ManagedCredential{
			Name:               existing.Name,
			Kind:               CredentialKind(existing.Kind),
			Username:           existing.Username,
			Password:           string(plaintext),
			SwarmSecretName:    existing.SwarmSecretName,
			NewlyCreated:       false,
			SwarmSecretCreated: secretCreated,
		}, nil
	}
	if err != store.ErrCredentialNotFound {
		return nil, fmt.Errorf("lookup %s: %w", spec.name, err)
	}

	// Brand new credential — generate, persist, mirror to Swarm.
	password, err := RandomPassword()
	if err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}
	ciphertext, err := m.Cipher.Encrypt([]byte(password))
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	if err := m.Store.InsertCredential(ctx, &store.ManagedCredential{
		Name:               spec.name,
		Kind:               string(spec.kind),
		Username:           spec.username,
		PasswordCiphertext: ciphertext,
		SwarmSecretName:    spec.swarmSecretName,
	}); err != nil {
		return nil, fmt.Errorf("insert credential %s: %w", spec.name, err)
	}
	secretPayload, err := serialisePassword(spec, spec.username, password)
	if err != nil {
		return nil, err
	}
	secretCreated, err := EnsureSecret(ctx, m.Docker, spec.swarmSecretName, secretPayload)
	if err != nil {
		return nil, fmt.Errorf("create swarm secret %s: %w", spec.swarmSecretName, err)
	}
	return &ManagedCredential{
		Name:               spec.name,
		Kind:               spec.kind,
		Username:           spec.username,
		Password:           password,
		SwarmSecretName:    spec.swarmSecretName,
		NewlyCreated:       true,
		SwarmSecretCreated: secretCreated,
	}, nil
}

// Rotate generates a new random password for an existing managed credential,
// updates the encrypted store row, removes the old Swarm secret, creates a
// fresh one with the new value, and force-restarts the consuming service so
// it re-mounts the new secret.
//
// Returns the new ManagedCredential with plaintext (callers should print it
// once and discard).
//
// Failure modes:
//   - Credential not in store → store.ErrCredentialNotFound
//   - Old Swarm secret cannot be removed (in use by an unknown service) →
//     wrapped error; the encrypted store has already been updated, but Swarm
//     still has the old value. Operator can re-run after addressing the
//     conflict.
func (m *CredentialsManager) Rotate(ctx context.Context, name string) (*ManagedCredential, error) {
	existing, err := m.Store.GetCredential(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("lookup %s: %w", name, err)
	}

	password, err := RandomPassword()
	if err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}
	ciphertext, err := m.Cipher.Encrypt([]byte(password))
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	if err := m.Store.RotateCredential(ctx, name, ciphertext); err != nil {
		return nil, fmt.Errorf("update credential row: %w", err)
	}

	// Reconstruct the spec so we know how to serialise the new password
	// (htpasswd vs plain) — same logic the bootstrap path uses.
	spec, ok := specFor(name, existing)
	if !ok {
		return nil, fmt.Errorf("rotate %s: no spec for credential kind %q", name, existing.Kind)
	}
	payload, err := serialisePassword(spec, existing.Username, password)
	if err != nil {
		return nil, err
	}

	// Swarm-side rotation: remove old secret, create new with same name.
	// Order matters — Docker refuses to remove a secret in use, so this
	// fails loud if the consuming service is still running. Operator
	// must scale the service down first or the rotation aborts here.
	if err := m.Docker.SecretRemove(ctx, existing.SwarmSecretName); err != nil {
		return nil, fmt.Errorf("remove old swarm secret %s (is the consuming service still using it?): %w", existing.SwarmSecretName, err)
	}
	if _, err := EnsureSecret(ctx, m.Docker, existing.SwarmSecretName, payload); err != nil {
		return nil, fmt.Errorf("re-create swarm secret %s: %w", existing.SwarmSecretName, err)
	}

	// Force-restart the consuming service so its tasks pick up the new
	// secret on next start. Best-effort: if no service mapping exists, log
	// and skip — the new secret is in place and will be picked up on the
	// next deploy.
	if svc, ok := consumingService[name]; ok && m.Deployer != nil {
		if err := m.Deployer.ForceUpdateService(ctx, svc); err != nil {
			return nil, fmt.Errorf("force-restart %s: %w (new secret IS in place; operator may restart manually)", svc, err)
		}
	}

	return &ManagedCredential{
		Name:               existing.Name,
		Kind:               CredentialKind(existing.Kind),
		Username:           existing.Username,
		Password:           password,
		SwarmSecretName:    existing.SwarmSecretName,
		NewlyCreated:       false,
		SwarmSecretCreated: true,
	}, nil
}

// specFor reconstructs the bootstrapSpec for an existing credential row.
// Used by Rotate so it knows whether to serialise as plain or htpasswd.
func specFor(name string, c *store.ManagedCredential) (bootstrapSpec, bool) {
	for _, s := range bootstrapSpecs() {
		if s.name == name {
			return s, true
		}
	}
	return bootstrapSpec{}, false
}

// bootstrapSpecs returns the canonical list of managed credentials.
// Single source of truth used by both Bootstrap (creates them) and
// specFor (looks one up by name during rotate).
func bootstrapSpecs() []bootstrapSpec {
	return []bootstrapSpec{
		{
			name:            "traefik_dashboard",
			kind:            KindTraefikAdmin,
			swarmSecretName: "admin_credentials",
			format:          formatHtpasswd,
		},
		{
			name:            "portainer",
			kind:            KindPortainer,
			swarmSecretName: "portainer_admin_password",
			format:          formatPlain,
		},
		{
			name:            "openobserve_admin",
			kind:            KindOpenObserve,
			swarmSecretName: "zo_root_user_password",
			format:          formatPlain,
		},
	}
}

// serialisePassword shapes the credential payload for its Swarm secret.
// Plain credentials emit the password bytes; htpasswd credentials emit
// "user:bcrypt(password)\n".
func serialisePassword(spec bootstrapSpec, username, password string) ([]byte, error) {
	switch spec.format {
	case formatPlain:
		return []byte(password), nil
	case formatHtpasswd:
		line, err := HtpasswdLine(username, password)
		if err != nil {
			return nil, err
		}
		return []byte(line), nil
	default:
		return nil, fmt.Errorf("unknown secret format %d", spec.format)
	}
}

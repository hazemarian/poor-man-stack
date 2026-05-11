package cluster

import (
	"context"
	"fmt"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// CredentialKind tags a managed credential with its consumer; drives the
// secret's payload format (plain vs htpasswd).
type CredentialKind string

const (
	KindTraefikAdmin CredentialKind = "traefik"
	KindPortainer    CredentialKind = "portainer"
	KindOpenObserve  CredentialKind = "openobserve"
)

// ManagedCredential is the in-memory shape of a bootstrap credential. The
// two "Created" flags diverge only in the "lost DB, kept Swarm" recovery
// scenario (fresh DB but a Swarm secret with that name already existed).
type ManagedCredential struct {
	Name               string
	Kind               CredentialKind
	Username           string
	Password           string
	SwarmSecretName    string
	NewlyCreated       bool
	SwarmSecretCreated bool
}

type BootstrapInput struct {
	TraefikAdminUser      string
	OpenObserveAdminEmail string
}

type CredentialsManager struct {
	Store    *store.Store
	Cipher   *credentials.Cipher
	Docker   docker.Client
	Deployer StackDeployer // only required for Rotate's force-restart step
}

// consumingService maps each managed credential to the swarm service that
// mounts its secret. Names are "<stack>_<service>" — what `docker stack
// deploy` produces. Keep in sync with the bundled stacks.
var consumingService = map[string]string{
	"traefik_dashboard": "infra_traefik",
	"portainer":         "infra_portainer",
	"openobserve_admin": "observability_openobserve",
}

// Bootstrap ensures every bundled component has a credential. Idempotent:
// re-runs preserve existing values, only missing entries are created.
// Returned plaintext passwords are decrypted from the store on re-runs so
// downstream renderers (e.g. the OTel config's basic-auth header) work.
func (m *CredentialsManager) Bootstrap(ctx context.Context, in BootstrapInput) (map[string]*ManagedCredential, error) {
	if in.TraefikAdminUser == "" {
		in.TraefikAdminUser = "admin"
	}
	if in.OpenObserveAdminEmail == "" {
		return nil, fmt.Errorf("OpenObserve admin email is required (use --openobserve-email or set in config)")
	}

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

type secretFormat int

const (
	formatPlain    secretFormat = iota // raw password bytes
	formatHtpasswd                     // "user:bcrypt(password)\n"
)

type bootstrapSpec struct {
	name            string
	kind            CredentialKind
	username        string
	swarmSecretName string
	format          secretFormat
}

// ensure is the get-or-create primitive: load from store and reconcile
// the matching Swarm secret, or mint a fresh password when missing.
func (m *CredentialsManager) ensure(ctx context.Context, spec bootstrapSpec) (*ManagedCredential, error) {
	existing, err := m.Store.GetCredential(ctx, spec.name)
	if err == nil {
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

// Rotate mints a new password, re-encrypts in store, swaps the Swarm
// secret, and force-restarts the consuming service. Returns plaintext
// (caller prints once and discards).
//
// If SecretRemove fails (typically: the secret is still mounted by a
// running service), the store has already been updated but Swarm still
// has the old value. Operator scales the consuming service to 0 and
// re-runs.
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

	spec, ok := specFor(name, existing)
	if !ok {
		return nil, fmt.Errorf("rotate %s: no spec for credential kind %q", name, existing.Kind)
	}
	payload, err := serialisePassword(spec, existing.Username, password)
	if err != nil {
		return nil, err
	}

	// Order matters: Docker refuses to remove a secret in use, so this
	// fails loud if the consuming service is still running.
	if err := m.Docker.SecretRemove(ctx, existing.SwarmSecretName); err != nil {
		return nil, fmt.Errorf("remove old swarm secret %s (is the consuming service still using it?): %w", existing.SwarmSecretName, err)
	}
	if _, err := EnsureSecret(ctx, m.Docker, existing.SwarmSecretName, payload); err != nil {
		return nil, fmt.Errorf("re-create swarm secret %s: %w", existing.SwarmSecretName, err)
	}

	// Best-effort: when no service mapping exists, the secret is still in
	// place and will be picked up on the next deploy.
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

func specFor(name string, _ *store.ManagedCredential) (bootstrapSpec, bool) {
	for _, s := range bootstrapSpecs() {
		if s.name == name {
			return s, true
		}
	}
	return bootstrapSpec{}, false
}

// bootstrapSpecs is the canonical list of managed credentials, used by
// both Bootstrap and Rotate.
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

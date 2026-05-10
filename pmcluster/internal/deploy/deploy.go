// Package deploy is the shared engine behind every "deploy a stack" path:
// the REST API (POST /api/stacks), the webhook receiver (POST /webhook/...),
// and the CLI (`pmcluster deploy <file>`). They all build the same Payload
// and call Service.Deploy.
//
// The pipeline:
//
//	Payload → Parse → (override version) → Interpolate → Validate → Translate
//	                → RecordDeploy (SQLite) → DeployStack (docker stack deploy)
//
// Returns the assigned revision id (unix timestamp) and the rendered YAML
// so callers can show / log / audit it.
package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/manifest"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// BackupTrigger runs an on-demand backup. Implemented by internal/backup;
// abstracted here so tests can stub it without spinning up offen.
type BackupTrigger interface {
	Trigger(ctx context.Context) (archivePaths []string, err error)
}

// Payload is the canonical shape of a deploy request. The webhook receiver
// and the REST API both decode JSON into this struct; the CLI builds it
// from --flags + a YAML file.
type Payload struct {
	// AppName overrides the manifest's `app:` field. Optional. Lets the same
	// manifest deploy under different stack names (multi-tenant scenarios).
	AppName string `json:"app_name,omitempty"`

	// RepoURL is metadata only — pmcluster never fetches from git. Stored
	// on the stack row for UI/audit.
	RepoURL string `json:"repo_url,omitempty"`

	// Version overrides the manifest's `version:` field (typically the
	// container image tag). Optional.
	Version string `json:"version,omitempty"`

	// Manifest is the YAML body of the DSL document. Required.
	Manifest string `json:"manifest"`
}

// DeployResult is what Deploy returns on success. Used by API/CLI to
// render confirmation to the operator.
type DeployResult struct {
	StackName    string
	Revision     int64
	RenderedYAML []byte
}

// Service bundles the dependencies the deploy pipeline needs. Constructed
// once per HTTP request OR once per CLI invocation.
type Service struct {
	Store    *store.Store
	Deployer cluster.StackDeployer

	// Backup is optional. When nil, manifests with backup_before_deploy: true
	// log a warning instead of failing — the deploy still proceeds. Wired
	// from the CLI/serve to internal/backup.
	Backup BackupTrigger
}

// Deploy runs the full pipeline for a brand-new revision. Called by both
// the API handler and the CLI deploy command.
//
// Validation errors are returned as-is (the caller surfaces them to the
// operator). Storage and deploy errors are wrapped with context.
func (s *Service) Deploy(ctx context.Context, p Payload) (*DeployResult, error) {
	if p.Manifest == "" {
		return nil, fmt.Errorf("manifest: required")
	}

	app, err := manifest.Parse([]byte(p.Manifest))
	if err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if p.AppName != "" {
		app.Name = p.AppName
	}
	if p.Version != "" {
		app.Version = p.Version
	}
	if err := manifest.Interpolate(app); err != nil {
		return nil, fmt.Errorf("interpolate: %w", err)
	}
	if err := manifest.Validate(app); err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}

	rendered, err := manifest.Translate(app)
	if err != nil {
		return nil, fmt.Errorf("translate: %w", err)
	}

	revision, err := s.Store.NextFreeRevision(ctx, app.Name, time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("assign revision: %w", err)
	}
	payloadJSON, _ := json.Marshal(p) // best-effort; never errors for our shape
	rev := &store.StackRevision{
		StackName:    app.Name,
		Revision:     revision,
		SourceYAML:   p.Manifest,
		RenderedYAML: string(rendered),
		PayloadJSON:  sql.NullString{String: string(payloadJSON), Valid: true},
	}
	if err := s.Store.RecordDeploy(ctx, rev, p.RepoURL); err != nil {
		return nil, fmt.Errorf("record deploy: %w", err)
	}

	// Pre-deploy backup hook. Best-effort: a flaky backup container should
	// never block an urgent deploy. Operator sees the failure in the
	// backups audit table and in the deploy output.
	if app.BackupBeforeDeploy {
		s.runPreDeployBackup(ctx, app.Name, revision)
	}

	if err := s.Deployer.DeployStack(ctx, app.Name, rendered); err != nil {
		// The stack row is now in a "recorded but not applied" state. We
		// intentionally don't roll it back — the next run can see what was
		// attempted. Operator can re-deploy or rollback.
		return nil, fmt.Errorf("docker stack deploy: %w", err)
	}

	return &DeployResult{
		StackName:    app.Name,
		Revision:     revision,
		RenderedYAML: rendered,
	}, nil
}

// Rollback re-applies a stored revision as a NEW revision (so the audit
// trail records both deploys, not just an arbitrary "current" pointer).
//
// Source/Rendered YAML are copied verbatim from the source revision; the
// new revision id is a fresh timestamp. PayloadJSON gets a synthetic
// rollback marker so it's distinguishable from forward deploys.
func (s *Service) Rollback(ctx context.Context, stackName string, sourceRevision int64) (*DeployResult, error) {
	src, err := s.Store.GetRevision(ctx, stackName, sourceRevision)
	if err != nil {
		return nil, err
	}

	revision, err := s.Store.NextFreeRevision(ctx, stackName, time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("assign revision: %w", err)
	}
	rolledBackPayload, _ := json.Marshal(map[string]any{
		"rollback_of": sourceRevision,
		"original":    json.RawMessage(src.PayloadJSON.String),
	})

	rev := &store.StackRevision{
		StackName:    stackName,
		Revision:     revision,
		SourceYAML:   src.SourceYAML,
		RenderedYAML: src.RenderedYAML,
		PayloadJSON:  sql.NullString{String: string(rolledBackPayload), Valid: true},
	}
	if err := s.Store.RecordDeploy(ctx, rev, ""); err != nil {
		return nil, fmt.Errorf("record rollback: %w", err)
	}

	if err := s.Deployer.DeployStack(ctx, stackName, []byte(src.RenderedYAML)); err != nil {
		return nil, fmt.Errorf("docker stack deploy (rollback): %w", err)
	}

	return &DeployResult{
		StackName:    stackName,
		Revision:     revision,
		RenderedYAML: []byte(src.RenderedYAML),
	}, nil
}

// runPreDeployBackup triggers a backup and records its outcome. Errors are
// swallowed — recorded in the audit table and surfaced via the next
// `pmcluster backup list`, but never fail the deploy.
func (s *Service) runPreDeployBackup(ctx context.Context, stackName string, revision int64) {
	id, err := s.Store.CreateBackup(ctx, stackName, revision)
	if err != nil {
		// Audit-log insert failed — log via the deploy result is not an
		// option here, so just bail. The deploy continues.
		return
	}
	if s.Backup == nil {
		_ = s.Store.FinishBackup(ctx, id, "failed", "", "no BackupTrigger configured (deploy.Service.Backup is nil)")
		return
	}
	paths, err := s.Backup.Trigger(ctx)
	if err != nil {
		_ = s.Store.FinishBackup(ctx, id, "failed", strings.Join(paths, ","), err.Error())
		return
	}
	_ = s.Store.FinishBackup(ctx, id, "succeeded", strings.Join(paths, ","), "")
}

// Package deploy is the shared engine behind the REST, webhook, and CLI
// deploy paths. Pipeline:
//
//	Payload → Parse → (override version) → Interpolate → Validate
//	        → Translate → RecordDeploy → DeployStack
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

// BackupTrigger is implemented by internal/backup; abstracted so tests
// can stub it without spinning up offen.
type BackupTrigger interface {
	Trigger(ctx context.Context) (archivePaths []string, err error)
}

// Payload is the canonical deploy request. JSON shape is shared by the
// REST handler and the webhook receiver.
type Payload struct {
	AppName  string `json:"app_name,omitempty"` // overrides manifest's `app:`
	RepoURL  string `json:"repo_url,omitempty"` // metadata only, never fetched
	Version  string `json:"version,omitempty"`  // overrides manifest's `version:`
	Manifest string `json:"manifest"`
}

type DeployResult struct {
	StackName    string
	Revision     int64
	RenderedYAML []byte
}

// Service bundles deploy-pipeline dependencies. Backup is optional;
// manifests with backup_before_deploy still proceed when nil.
type Service struct {
	Store    *store.Store
	Deployer cluster.StackDeployer
	Backup   BackupTrigger
}

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
	payloadJSON, _ := json.Marshal(p)
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

	// Best-effort: a flaky backup never blocks the deploy.
	if app.BackupBeforeDeploy {
		s.runPreDeployBackup(ctx, app.Name, revision)
	}

	if err := s.Deployer.DeployStack(ctx, app.Name, rendered); err != nil {
		// "Recorded but not applied" is intentional — operator can see
		// what was attempted and re-deploy or rollback.
		return nil, fmt.Errorf("docker stack deploy: %w", err)
	}

	return &DeployResult{
		StackName:    app.Name,
		Revision:     revision,
		RenderedYAML: rendered,
	}, nil
}

// Rollback re-applies a stored revision as a NEW revision so the audit
// trail records both deploys. PayloadJSON carries a rollback_of marker.
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

// runPreDeployBackup records its outcome in the audit table; never blocks.
func (s *Service) runPreDeployBackup(ctx context.Context, stackName string, revision int64) {
	id, err := s.Store.CreateBackup(ctx, stackName, revision)
	if err != nil {
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

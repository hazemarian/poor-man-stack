package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Stack is a deployed application's metadata. The actual compose contents
// live in StackRevision rows, keyed by unix-timestamp revision id.
type Stack struct {
	Name            string
	CurrentRevision int64
	RepoURL         sql.NullString
	CreatedAt       int64
	UpdatedAt       int64
}

// StackRevision is one historical (or current) deploy of a stack.
// SourceYAML is the operator-submitted DSL; RenderedYAML is what pmcluster
// piped to `docker stack deploy`. PayloadJSON captures the full webhook/API
// envelope (for audit/UI).
type StackRevision struct {
	StackName    string
	Revision     int64
	SourceYAML   string
	RenderedYAML string
	PayloadJSON  sql.NullString
	CreatedAt    int64
}

// ErrStackNotFound is returned by GetStack/GetRevision when the row is missing.
var ErrStackNotFound = errors.New("stack not found")

// ErrRevisionNotFound is returned by GetRevision when the (stack, revision) pair is missing.
var ErrRevisionNotFound = errors.New("revision not found")

// RecordDeploy is the canonical "I just deployed something" call. Atomic:
// inserts the new revision row AND upserts the stack row's current_revision
// pointer. revision must be a unix timestamp produced by the caller (so the
// deploy service can stamp source/rendered together with the same id).
//
// On a brand-new stack, the row is inserted with created_at=updated_at=now.
// On a re-deploy, only updated_at moves.
func (s *Store) RecordDeploy(ctx context.Context, rev *StackRevision, repoURL string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()

	// Insert the revision row first so the FK target on stacks(name) is
	// satisfied by the time we touch the parent.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO stack_revisions
		   (stack_name, revision, source_yaml, rendered_yaml, payload_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		rev.StackName, rev.Revision, rev.SourceYAML, rev.RenderedYAML,
		nullableString(rev.PayloadJSON.String, rev.PayloadJSON.Valid), now,
	); err != nil {
		// FK isn't satisfied yet (this is the first deploy for this stack)
		// — fall through and insert the parent first, then retry.
		if !isForeignKeyViolation(err) {
			return fmt.Errorf("insert revision: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stacks (name, current_revision, repo_url, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?)`,
			rev.StackName, rev.Revision, nullableString(repoURL, repoURL != ""), now, now,
		); err != nil {
			return fmt.Errorf("insert stack: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stack_revisions
			   (stack_name, revision, source_yaml, rendered_yaml, payload_json, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			rev.StackName, rev.Revision, rev.SourceYAML, rev.RenderedYAML,
			nullableString(rev.PayloadJSON.String, rev.PayloadJSON.Valid), now,
		); err != nil {
			return fmt.Errorf("insert revision (after parent): %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		return nil
	}

	// Revision insert succeeded → stack row already exists. Update pointer.
	res, err := tx.ExecContext(ctx,
		`UPDATE stacks SET current_revision = ?, updated_at = ?, repo_url = COALESCE(NULLIF(?, ''), repo_url)
		 WHERE name = ?`,
		rev.Revision, now, repoURL, rev.StackName,
	)
	if err != nil {
		return fmt.Errorf("update stack: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		// Defensive: revision insert worked (so FK was OK) but stack
		// disappeared. Race with a delete? Re-create.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stacks (name, current_revision, repo_url, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?)`,
			rev.StackName, rev.Revision, nullableString(repoURL, repoURL != ""), now, now,
		); err != nil {
			return fmt.Errorf("re-insert stack: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// GetStack fetches a stack row by name.
func (s *Store) GetStack(ctx context.Context, name string) (*Stack, error) {
	var st Stack
	err := s.db.QueryRowContext(ctx,
		`SELECT name, current_revision, repo_url, created_at, updated_at FROM stacks WHERE name = ?`, name,
	).Scan(&st.Name, &st.CurrentRevision, &st.RepoURL, &st.CreatedAt, &st.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrStackNotFound
		}
		return nil, fmt.Errorf("query stack: %w", err)
	}
	return &st, nil
}

// ListStacks returns all stacks ordered by name.
func (s *Store) ListStacks(ctx context.Context) ([]*Stack, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, current_revision, repo_url, created_at, updated_at FROM stacks ORDER BY name`,
	)
	if err != nil {
		return nil, fmt.Errorf("query stacks: %w", err)
	}
	defer rows.Close()
	var out []*Stack
	for rows.Next() {
		var st Stack
		if err := rows.Scan(&st.Name, &st.CurrentRevision, &st.RepoURL, &st.CreatedAt, &st.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan stack: %w", err)
		}
		out = append(out, &st)
	}
	return out, rows.Err()
}

// NextFreeRevision returns the smallest revision id ≥ candidate that is
// NOT already used for stackName. Lets callers (deploy + rollback)
// generate a unix-timestamp candidate and survive sub-second collisions
// (back-to-back deploys/rollbacks in the same second).
//
// Bounded scan: caps at candidate+10000 to avoid pathological loops if
// something is very wrong. In practice we exit on the second iteration
// at most.
func (s *Store) NextFreeRevision(ctx context.Context, stackName string, candidate int64) (int64, error) {
	const maxAttempts = 10000
	for i := int64(0); i < maxAttempts; i++ {
		r := candidate + i
		var dummy int64
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM stack_revisions WHERE stack_name = ? AND revision = ?`,
			stackName, r,
		).Scan(&dummy)
		if errors.Is(err, sql.ErrNoRows) {
			return r, nil
		}
		if err != nil {
			return 0, fmt.Errorf("check revision %d: %w", r, err)
		}
	}
	return 0, fmt.Errorf("could not find a free revision for %s near %d after %d attempts", stackName, candidate, maxAttempts)
}

// GetRevision fetches one specific revision (used by the rollback flow,
// which loads the stored RenderedYAML and re-applies it as a new revision).
func (s *Store) GetRevision(ctx context.Context, stackName string, revision int64) (*StackRevision, error) {
	var r StackRevision
	err := s.db.QueryRowContext(ctx,
		`SELECT stack_name, revision, source_yaml, rendered_yaml, payload_json, created_at
		 FROM stack_revisions WHERE stack_name = ? AND revision = ?`,
		stackName, revision,
	).Scan(&r.StackName, &r.Revision, &r.SourceYAML, &r.RenderedYAML, &r.PayloadJSON, &r.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRevisionNotFound
		}
		return nil, fmt.Errorf("query revision: %w", err)
	}
	return &r, nil
}

// ListRevisions returns up to `limit` most recent revisions for a stack,
// newest first. limit ≤ 0 means "all".
func (s *Store) ListRevisions(ctx context.Context, stackName string, limit int) ([]*StackRevision, error) {
	var rows *sql.Rows
	var err error
	if limit > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT stack_name, revision, source_yaml, rendered_yaml, payload_json, created_at
			 FROM stack_revisions WHERE stack_name = ? ORDER BY revision DESC LIMIT ?`,
			stackName, limit,
		)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT stack_name, revision, source_yaml, rendered_yaml, payload_json, created_at
			 FROM stack_revisions WHERE stack_name = ? ORDER BY revision DESC`,
			stackName,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("query revisions: %w", err)
	}
	defer rows.Close()
	var out []*StackRevision
	for rows.Next() {
		var r StackRevision
		if err := rows.Scan(&r.StackName, &r.Revision, &r.SourceYAML, &r.RenderedYAML, &r.PayloadJSON, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan revision: %w", err)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// nullableString turns ("foo", true) → sql.NullString{Foo, true} as a
// driver argument. ("anything", false) → driver-side NULL.
func nullableString(s string, valid bool) any {
	if !valid {
		return nil
	}
	return s
}

// isForeignKeyViolation matches modernc.org/sqlite's FK error string.
func isForeignKeyViolation(err error) bool {
	return err != nil && containsAny(err.Error(),
		"FOREIGN KEY constraint failed",
		"foreign key constraint failed",
	)
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
)

// ErrUserExists is returned by CreateUser when the name is already taken.
var ErrUserExists = errors.New("user already exists")

// CreateUser stores a new user with the given pre-hashed token. Returns the
// row id. The hashing is the caller's responsibility (auth.HashToken) so
// store doesn't depend on auth's choice of algorithm — keeps the layering
// honest if we ever swap argon2id for something else.
func (s *Store) CreateUser(ctx context.Context, name, tokenHash string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (name, token_hash, created_at) VALUES (?, ?, ?)`,
		name, tokenHash, time.Now().Unix(),
	)
	if err != nil {
		// modernc.org/sqlite returns "constraint failed: UNIQUE" — use the
		// generic substring rather than coupling to a driver-specific error type.
		if isUniqueViolation(err) {
			return 0, ErrUserExists
		}
		return 0, fmt.Errorf("insert user: %w", err)
	}
	return res.LastInsertId()
}

// CountUsers returns the total number of users — used by `pmcluster init` to
// detect "fresh database, allow bootstrap" vs "existing data, refuse without --force".
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// UserByToken implements auth.Lookup. Iterates all users and verifies the
// token against each stored hash; returns the matching user or (nil, nil).
//
// O(N) on user count is fine for the single-digit user counts pmcluster
// expects. If we ever blow past that, see the note on auth.Lookup.
func (s *Store) UserByToken(ctx context.Context, token string) (*auth.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, token_hash FROM users`)
	if err != nil {
		return nil, fmt.Errorf("query users: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			u    auth.User
			hash string
		)
		if err := rows.Scan(&u.ID, &u.Name, &hash); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		ok, err := auth.VerifyToken(token, hash)
		if err != nil {
			// One bad row shouldn't lock everyone out; log via caller and skip.
			continue
		}
		if ok {
			return &u, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	return nil, nil
}

// UserByID looks up a user by primary key. Returns (nil, sql.ErrNoRows) if
// not found — callers should check with errors.Is.
func (s *Store) UserByID(ctx context.Context, id int64) (*auth.User, error) {
	var u auth.User
	err := s.db.QueryRowContext(ctx, `SELECT id, name FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("query user: %w", err)
	}
	return &u, nil
}

func isUniqueViolation(err error) bool {
	// modernc.org/sqlite's error string for UNIQUE constraint violations.
	// Verified empirically; brittle enough that we keep it isolated here.
	return err != nil && (containsAny(err.Error(),
		"UNIQUE constraint failed",
		"constraint failed: UNIQUE",
	))
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

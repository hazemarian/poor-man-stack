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

// CreateUser stores a pre-hashed token; hashing is the caller's job
// (auth.HashToken) so store doesn't pin auth's algorithm.
func (s *Store) CreateUser(ctx context.Context, name, tokenHash string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (name, token_hash, created_at) VALUES (?, ?, ?)`,
		name, tokenHash, time.Now().Unix(),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrUserExists
		}
		return 0, fmt.Errorf("insert user: %w", err)
	}
	return res.LastInsertId()
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// UserByToken iterates all users (argon2id salts are per-row, so no index
// is possible). O(N) is fine at single-digit user counts.
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
			// A single bad row mustn't lock out every other user.
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

// UserByID returns (nil, sql.ErrNoRows) when not found.
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

// isUniqueViolation matches modernc.org/sqlite's UNIQUE error strings.
// Brittle; kept isolated here so a driver swap is a one-liner.
func isUniqueViolation(err error) bool {
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

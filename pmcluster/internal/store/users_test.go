package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
)

// openTestStore is a helper that opens a fresh in-temp-dir store and registers
// cleanup. Every test gets its own isolated DB.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// hashFor is a test helper that produces a real argon2id hash for a token.
func hashFor(t *testing.T, token string) string {
	t.Helper()
	h, err := auth.HashToken(token)
	if err != nil {
		t.Fatalf("HashToken(%q): %v", token, err)
	}
	return h
}

// TestCreateUser_HappyPath verifies that a new user is inserted and returns a
// valid (non-zero) row ID.
func TestCreateUser_HappyPath(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, err := s.CreateUser(ctx, "alice", hashFor(t, "token-alice"))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if id <= 0 {
		t.Errorf("CreateUser returned id=%d, want > 0", id)
	}
}

// TestCreateUser_UniqueCollision verifies that inserting a duplicate name
// returns ErrUserExists and not a raw DB error.
func TestCreateUser_UniqueCollision(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, err := s.CreateUser(ctx, "alice", hashFor(t, "token-a"))
	if err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}
	_, err = s.CreateUser(ctx, "alice", hashFor(t, "token-b"))
	if !errors.Is(err, ErrUserExists) {
		t.Errorf("second CreateUser err = %v, want ErrUserExists", err)
	}
}

// TestCountUsers covers the empty-table and populated-table cases.
func TestCountUsers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	n, err := s.CountUsers(ctx)
	if err != nil {
		t.Fatalf("CountUsers (empty): %v", err)
	}
	if n != 0 {
		t.Errorf("CountUsers on empty DB = %d, want 0", n)
	}

	if _, err := s.CreateUser(ctx, "alice", hashFor(t, "tok-a")); err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	if _, err := s.CreateUser(ctx, "bob", hashFor(t, "tok-b")); err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	n, err = s.CountUsers(ctx)
	if err != nil {
		t.Fatalf("CountUsers (populated): %v", err)
	}
	if n != 2 {
		t.Errorf("CountUsers after 2 inserts = %d, want 2", n)
	}
}

// TestUserByToken covers: unknown token returns nil, known token returns the
// right user, and a multi-user scenario (N=3) where only the correct user is
// returned.
func TestUserByToken(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	t.Run("unknown token returns nil", func(t *testing.T) {
		u, err := s.UserByToken(ctx, "does-not-exist")
		if err != nil {
			t.Fatalf("UserByToken: %v", err)
		}
		if u != nil {
			t.Errorf("expected nil for unknown token, got %+v", u)
		}
	})

	// Set up three users with real argon2id hashes.
	tokens := []string{"tok-alice", "tok-bob", "tok-carol"}
	names := []string{"alice", "bob", "carol"}
	for i, name := range names {
		if _, err := s.CreateUser(ctx, name, hashFor(t, tokens[i])); err != nil {
			t.Fatalf("CreateUser %s: %v", name, err)
		}
	}

	t.Run("valid token returns the right user", func(t *testing.T) {
		u, err := s.UserByToken(ctx, "tok-alice")
		if err != nil {
			t.Fatalf("UserByToken: %v", err)
		}
		if u == nil {
			t.Fatal("expected user, got nil")
		}
		if u.Name != "alice" {
			t.Errorf("user.Name = %q, want %q", u.Name, "alice")
		}
	})

	t.Run("multi-user: each token maps to its own user", func(t *testing.T) {
		for i, tok := range tokens {
			u, err := s.UserByToken(ctx, tok)
			if err != nil {
				t.Fatalf("UserByToken(%q): %v", tok, err)
			}
			if u == nil {
				t.Fatalf("UserByToken(%q) = nil, want %q", tok, names[i])
			}
			if u.Name != names[i] {
				t.Errorf("UserByToken(%q).Name = %q, want %q", tok, u.Name, names[i])
			}
		}
	})

	t.Run("token for no user returns nil", func(t *testing.T) {
		u, err := s.UserByToken(ctx, "token-for-nobody")
		if err != nil {
			t.Fatalf("UserByToken: %v", err)
		}
		if u != nil {
			t.Errorf("expected nil for non-matching token, got %+v", u)
		}
	})
}

// TestUserByID covers the not-found (sql.ErrNoRows) path and the happy path.
func TestUserByID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	t.Run("unknown id returns sql.ErrNoRows", func(t *testing.T) {
		u, err := s.UserByID(ctx, 9999)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("UserByID(9999) err = %v, want sql.ErrNoRows", err)
		}
		if u != nil {
			t.Errorf("UserByID(9999) = %+v, want nil", u)
		}
	})

	t.Run("known id returns the right user", func(t *testing.T) {
		id, err := s.CreateUser(ctx, "dave", hashFor(t, "tok-dave"))
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		u, err := s.UserByID(ctx, id)
		if err != nil {
			t.Fatalf("UserByID(%d): %v", id, err)
		}
		if u == nil {
			t.Fatal("expected user, got nil")
		}
		if u.ID != id {
			t.Errorf("user.ID = %d, want %d", u.ID, id)
		}
		if u.Name != "dave" {
			t.Errorf("user.Name = %q, want dave", u.Name)
		}
	})
}

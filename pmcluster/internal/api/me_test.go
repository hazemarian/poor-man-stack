package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
)

// contextWithUser returns a context that has the given User attached the same
// way Bearer middleware attaches it. We reach into the unexported ctxKey by
// calling auth.FromContext in reverse — we must set it via context.WithValue
// using the same key type. Since ctxKey is unexported we use the exported
// Bearer middleware to set it, or we inject it through a helper handler.
//
// The cleanest approach for package-internal tests: build a minimal request
// through a one-off middleware that calls context.WithValue.

func requestWithUser(u *auth.User) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	// Use the same bearer middleware mechanism: wrap the request in a context
	// that has the user. We can use auth.Bearer to inject users for real, but
	// here we exercise Me directly by going through a thin wrapper.
	ctx := req.Context()
	// Inject the user the same way Bearer middleware does: via a one-use handler
	// chain. Since ctxKey is unexported in auth, we call Bearer with a fake
	// lookup that always returns u.
	return req.WithContext(contextWithAuthUser(ctx, u))
}

// contextWithAuthUser injects a user into a context using the same key that
// auth.Bearer and auth.FromContext use. We have to route through Bearer
// middleware since ctxKey is unexported.
func contextWithAuthUser(ctx context.Context, u *auth.User) context.Context {
	// Re-use the Bearer middleware's path indirectly: run a tiny HTTP exchange
	// so Bearer injects the user, capture the resulting context.
	var captured context.Context
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = r.Context()
	})
	middleware := auth.Bearer(staticLookup{u})
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	if u != nil {
		req.Header.Set("Authorization", "Bearer dummy-token")
	}
	w := httptest.NewRecorder()
	middleware(inner).ServeHTTP(w, req)
	if captured != nil {
		return captured
	}
	return ctx
}

// staticLookup always returns the same user for any token.
type staticLookup struct{ user *auth.User }

func (s staticLookup) UserByToken(_ context.Context, _ string) (*auth.User, error) {
	return s.user, nil
}

// TestMe_AuthenticatedUser verifies Me returns 200 + correct JSON when a user
// is in the context (simulates Bearer middleware already having run).
func TestMe_AuthenticatedUser(t *testing.T) {
	u := &auth.User{ID: 7, Name: "testuser"}
	req := requestWithUser(u)
	rec := httptest.NewRecorder()

	Me(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["name"] != "testuser" {
		t.Errorf("name = %v, want testuser", body["name"])
	}
	if body["id"].(float64) != 7 {
		t.Errorf("id = %v, want 7", body["id"])
	}
}

// TestMe_MissingContextUser verifies the defensive 500 path when Me is called
// without Bearer middleware — i.e. auth.FromContext returns nil.
func TestMe_MissingContextUser(t *testing.T) {
	// Plain request with no user in context.
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	rec := httptest.NewRecorder()

	Me(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

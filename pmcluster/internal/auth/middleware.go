package auth

import (
	"context"
	"net/http"
	"strings"
)

// User is the authenticated principal attached to the request context by
// Bearer middleware. Kept minimal — Phase 1 only needs name to power
// /api/me; richer authz lives in later phases.
type User struct {
	ID   int64
	Name string
}

// ctxKey is unexported to prevent context-key collisions across packages.
type ctxKey struct{}

// FromContext retrieves the authenticated user, if any. Handlers behind
// Bearer middleware can rely on it being non-nil; handlers reachable
// without auth must check.
func FromContext(ctx context.Context) *User {
	u, _ := ctx.Value(ctxKey{}).(*User)
	return u
}

// Lookup is the contract Bearer needs from a user store: given a plaintext
// token, return the matching user (or nil, nil if no match).
//
// Implementations MUST iterate users and call VerifyToken — there's no way
// to look up by token directly because we only store hashes (and argon2id
// salts are per-row). For the small user counts pmcluster expects (single
// digits) this is fine; Phase 4+ may add a lookup index keyed by the first
// few token bytes if it ever becomes a hot path.
type Lookup interface {
	UserByToken(ctx context.Context, token string) (*User, error)
}

// Bearer returns middleware that requires `Authorization: Bearer <token>`
// and looks the token up via lookup. Missing/malformed headers and unknown
// tokens both return 401 with no body to avoid leaking which case it was.
func Bearer(lookup Lookup) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := extractBearer(r.Header.Get("Authorization"))
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="pmcluster"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			user, err := lookup.UserByToken(r.Context(), token)
			if err != nil || user == nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="pmcluster"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), ctxKey{}, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractBearer parses an "Authorization: Bearer <token>" header. Tolerant
// of trailing whitespace; case-insensitive on the scheme; rejects empty
// tokens and other schemes (Basic, etc.).
func extractBearer(header string) (string, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", false
	}
	const scheme = "bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(header[len(scheme):])
	if token == "" {
		return "", false
	}
	return token, true
}

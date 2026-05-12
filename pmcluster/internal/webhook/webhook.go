// Package webhook implements the HMAC-verified deploy webhook receiver.
//
// Endpoint: POST /webhook/{source}
//
//	Body:     deploy.Payload as JSON
//	Header:   X-Pmcluster-Signature: sha256=<hex>
//
// The signature is HMAC-SHA256 over the raw request body with the shared
// secret stored under the named source. Constant-time comparison on the
// hex-decoded digest. The endpoint is unauthenticated (no Bearer token);
// the HMAC IS the auth. CI systems can post freely as long as they hold
// the source's shared secret.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/deploy"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// SignatureHeader format: "sha256=<lowercase-hex>". Matches GitHub/GitLab
// so off-the-shelf CI integrations work.
const SignatureHeader = "X-Pmcluster-Signature"

// MaxBodyBytes caps the webhook body. Manifests are <10 KB typically;
// 1 MB leaves headroom and bounds HMAC compute cost from hostile callers.
const MaxBodyBytes = 1 << 20

type Handler struct {
	Store   *store.Store
	Cipher  *credentials.Cipher
	Service *deploy.Service
}

// Mount registers POST /webhook/{source}. Caller MUST place this outside
// the Bearer-protected /api subtree — HMAC IS the auth.
func (h *Handler) Mount(r chi.Router) {
	r.Post("/webhook/{source}", h.receive)
}

// HTTP status discipline:
//   - 401: any HMAC failure mode (missing/invalid sig, unknown source).
//     Same status for all so an attacker can't distinguish the cases.
//   - 400: body too large, malformed JSON, or deploy validation failure.
//   - 502: docker stack deploy returned an error.
func (h *Handler) receive(w http.ResponseWriter, r *http.Request) {
	source := chi.URLParam(r, "source")
	if source == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source required"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}
	if len(body) > MaxBodyBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "body too large"})
		return
	}

	if err := h.verifyHMAC(r.Context(), source, body, r.Header.Get(SignatureHeader)); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}

	// Mark used AFTER HMAC verifies, regardless of deploy outcome — useful
	// for "is this CI integration actually hitting us" (a malformed payload
	// from a known source still counts as proof-of-life).
	_ = h.Store.MarkWebhookSourceUsed(r.Context(), source)

	var p deploy.Payload
	if err := json.Unmarshal(body, &p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}

	res, err := h.Service.Deploy(r.Context(), p)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"stack":    res.StackName,
		"revision": res.Revision,
	})
}

// verifyHMAC returns a non-nil error for ANY failure mode. Caller maps
// every error to a single 401 to avoid leaking auth state.
func (h *Handler) verifyHMAC(ctx context.Context, source string, body []byte, sigHeader string) error {
	if sigHeader == "" {
		return errors.New("missing signature header")
	}
	parsed, ok := strings.CutPrefix(sigHeader, "sha256=")
	if !ok {
		return errors.New("signature must be sha256=<hex>")
	}
	want, err := hex.DecodeString(parsed)
	if err != nil || len(want) != sha256.Size {
		return errors.New("signature must be 64 hex chars after sha256=")
	}

	src, err := h.Store.GetWebhookSource(ctx, source)
	if err != nil {
		return err
	}

	secret, err := h.Cipher.Decrypt(src.SecretCiphertext)
	if err != nil {
		return fmt.Errorf("decrypt secret for %s: %w", source, err)
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	got := mac.Sum(nil)

	if !hmac.Equal(want, got) {
		return errors.New("signature mismatch")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

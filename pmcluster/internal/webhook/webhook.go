// Package webhook implements the HMAC-verified deploy webhook receiver.
//
// Endpoint: POST /webhook/{source}
//   Body:     deploy.Payload as JSON
//   Header:   X-Pmcluster-Signature: sha256=<hex>
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

// SignatureHeader is where callers MUST place their HMAC signature.
// Format: "sha256=<lowercase-hex>". Extra "sha256=" prefix is the same
// shape GitHub/GitLab use, so off-the-shelf CI integrations work.
const SignatureHeader = "X-Pmcluster-Signature"

// MaxBodyBytes is the upper bound on a webhook body. Manifests are tiny
// (<10 KB typically); 1 MB leaves comfortable headroom and prevents trivial
// memory-exhaustion attacks on the HMAC compute step.
const MaxBodyBytes = 1 << 20

// Handler bundles the dependencies the webhook receiver needs.
type Handler struct {
	Store        *store.Store
	Cipher       *credentials.Cipher
	Service      *deploy.Service
}

// Mount registers POST /webhook/{source}. Caller is responsible for placing
// this OUTSIDE the Bearer-protected /api/* subtree (no token required —
// HMAC IS the auth).
func (h *Handler) Mount(r chi.Router) {
	r.Post("/webhook/{source}", h.receive)
}

// receive verifies the HMAC, decodes the payload, calls deploy.Service,
// and renders a JSON response. All failure modes return JSON; success is
// 200 with {stack, revision}.
//
// HTTP status discipline:
//   - 401 — missing or invalid signature, OR unknown source. We deliberately
//           return the SAME status for both so an attacker can't tell the
//           difference between "source doesn't exist" and "wrong secret".
//   - 400 — body is too large, malformed JSON, or fails deploy validation
//           (returned by deploy.Service.Deploy with the original error message).
//   - 502 — Docker SDK / docker stack deploy returned an error.
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
		// SAME 401 for every failure mode — never reveal which of "unknown
		// source" / "missing signature" / "wrong digest" actually tripped.
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}

	// Mark used AFTER HMAC succeeds (regardless of whether the deploy
	// itself succeeds). This makes `pmcluster webhook list` useful for
	// "is this CI integration actually hitting us" — even a malformed
	// payload from a known source counts as proof-of-life.
	_ = h.Store.MarkWebhookSourceUsed(r.Context(), source)

	var p deploy.Payload
	if err := json.Unmarshal(body, &p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}

	res, err := h.Service.Deploy(r.Context(), p)
	if err != nil {
		// Bias toward 502 since the most likely source of error here is a
		// docker stack deploy failure. Manifest validation errors come
		// through this same bucket — operator inspects the message body.
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"stack":    res.StackName,
		"revision": res.Revision,
	})
}

// verifyHMAC fetches the source's secret, computes HMAC-SHA256 over the
// body, and compares it to the supplied signature in constant time.
//
// Returns a non-nil error for ANY failure (unknown source, missing
// signature, wrong format, mismatched digest). Caller surfaces a single
// 401 — never reveal which case it was.
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

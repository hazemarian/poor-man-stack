package auth

import (
	"strings"
	"testing"
)

// TestGenerateToken_RandomAndLongEnough is the smoke test the sub-agent
// (Phase 1.7) should expand: token entropy distribution, multiple gens
// don't collide over N runs, etc.
func TestGenerateToken_RandomAndLongEnough(t *testing.T) {
	a, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	b, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if a == b {
		t.Fatal("two consecutive tokens are identical (entropy issue?)")
	}
	if len(a) < 40 {
		t.Errorf("token too short: %d chars", len(a))
	}
}

func TestHashAndVerify_RoundTrip(t *testing.T) {
	tok, err := GenerateToken()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	h, err := HashToken(tok)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(h, hashPrefix) {
		t.Errorf("missing hash prefix: %s", h)
	}
	ok, err := VerifyToken(tok, h)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("VerifyToken returned false for matching pair")
	}
}

func TestVerifyToken_WrongSecret(t *testing.T) {
	h, err := HashToken("real-token")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ok, err := VerifyToken("wrong-token", h)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ok {
		t.Fatal("VerifyToken returned true for mismatched pair")
	}
}

func TestVerifyToken_MalformedHash(t *testing.T) {
	_, err := VerifyToken("any", "not-a-pmcluster-hash")
	if err != ErrInvalidHash {
		t.Errorf("err = %v, want ErrInvalidHash", err)
	}
}

func TestHashToken_EmptyRejected(t *testing.T) {
	if _, err := HashToken(""); err != ErrInvalidToken {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
}

func TestExtractBearer(t *testing.T) {
	cases := []struct {
		header     string
		wantToken  string
		wantOK     bool
	}{
		{"", "", false},
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"  Bearer   abc  ", "abc", true},
		{"Basic abc", "", false},
		{"Bearer", "", false},
		{"Bearer ", "", false},
	}
	for _, c := range cases {
		t.Run(c.header, func(t *testing.T) {
			tok, ok := extractBearer(c.header)
			if ok != c.wantOK || tok != c.wantToken {
				t.Errorf("extractBearer(%q) = (%q, %v), want (%q, %v)",
					c.header, tok, ok, c.wantToken, c.wantOK)
			}
		})
	}
}

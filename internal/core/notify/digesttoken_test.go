package notify

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/founderstack/api/internal/pkg/secret"
)

func TestDigestTokenSigner_RoundTrip(t *testing.T) {
	signer := NewDigestTokenSigner(secret.Value("test-digest-secret"))
	orgID := uuid.New()

	token := signer.Sign(orgID)
	if token == "" {
		t.Fatal("Sign() returned empty token with a configured secret")
	}

	gotOrgID, err := signer.Verify(token)
	if err != nil {
		t.Fatalf("Verify() error = %v, want nil", err)
	}
	if gotOrgID != orgID {
		t.Fatalf("Verify() orgID = %v, want %v", gotOrgID, orgID)
	}
}

func TestDigestTokenSigner_UnsetSecretRejectsEverything(t *testing.T) {
	signer := NewDigestTokenSigner(secret.Value(""))
	orgID := uuid.New()

	if token := signer.Sign(orgID); token != "" {
		t.Fatalf("Sign() with unset secret = %q, want empty", token)
	}

	// Even a token minted by a *different*, configured signer must be
	// rejected — an unset secret degrades to "no working unsubscribe
	// link", never "accept anything".
	signed := NewDigestTokenSigner(secret.Value("other-secret")).Sign(orgID)
	if _, err := signer.Verify(signed); err != ErrDigestTokenInvalid {
		t.Fatalf("Verify() error = %v, want ErrDigestTokenInvalid", err)
	}
}

func TestDigestTokenSigner_TamperedRejected(t *testing.T) {
	signer := NewDigestTokenSigner(secret.Value("test-digest-secret"))
	orgID := uuid.New()

	token := signer.Sign(orgID)
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("token has unexpected shape: %q", token)
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	sigBytes[0] ^= 0xFF
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(sigBytes)

	if _, err := signer.Verify(tampered); err != ErrDigestTokenInvalid {
		t.Fatalf("Verify() error = %v, want ErrDigestTokenInvalid", err)
	}
}

func TestDigestTokenSigner_MalformedTokenRejected(t *testing.T) {
	signer := NewDigestTokenSigner(secret.Value("test-digest-secret"))

	for _, tok := range []string{"", "not-a-token", "onlyonepart", "..", "a.b.c"} {
		if _, err := signer.Verify(tok); err != ErrDigestTokenInvalid {
			t.Errorf("Verify(%q) error = %v, want ErrDigestTokenInvalid", tok, err)
		}
	}
}

func TestDigestTokenSigner_WrongOrgTokenNotConfusedWithAnother(t *testing.T) {
	signer := NewDigestTokenSigner(secret.Value("test-digest-secret"))
	orgA, orgB := uuid.New(), uuid.New()

	tokenA := signer.Sign(orgA)
	gotOrgID, err := signer.Verify(tokenA)
	if err != nil {
		t.Fatalf("Verify() error = %v, want nil", err)
	}
	if gotOrgID == orgB {
		t.Fatal("Verify() returned orgB's id for a token signed for orgA")
	}
	if gotOrgID != orgA {
		t.Fatalf("Verify() orgID = %v, want %v", gotOrgID, orgA)
	}
}

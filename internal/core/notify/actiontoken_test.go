package notify

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/founderstack/api/internal/pkg/secret"
)

func TestActionTokenSigner_RoundTrip(t *testing.T) {
	signer := NewActionTokenSigner(secret.Value("test-secret"))
	approvalID, userID := uuid.New(), uuid.New()

	token := signer.Sign(approvalID, userID, time.Now().Add(time.Hour))
	if token == "" {
		t.Fatal("Sign() returned empty token with a configured secret")
	}

	gotUserID, err := signer.Verify(token, approvalID)
	if err != nil {
		t.Fatalf("Verify() error = %v, want nil", err)
	}
	if gotUserID != userID {
		t.Fatalf("Verify() userID = %v, want %v", gotUserID, userID)
	}
}

func TestActionTokenSigner_UnsetSecretRejectsEverything(t *testing.T) {
	signer := NewActionTokenSigner(secret.Value(""))
	approvalID, userID := uuid.New(), uuid.New()

	if token := signer.Sign(approvalID, userID, time.Now().Add(time.Hour)); token != "" {
		t.Fatalf("Sign() with unset secret = %q, want empty", token)
	}

	// Verify must reject even a token minted by a *different*, configured
	// signer — an unset secret degrades to "no working action buttons",
	// never "accept anything".
	signed := NewActionTokenSigner(secret.Value("other-secret")).Sign(approvalID, userID, time.Now().Add(time.Hour))
	if _, err := signer.Verify(signed, approvalID); err != ErrActionTokenInvalid {
		t.Fatalf("Verify() error = %v, want ErrActionTokenInvalid", err)
	}
}

func TestActionTokenSigner_ExpiredRejected(t *testing.T) {
	signer := NewActionTokenSigner(secret.Value("test-secret"))
	approvalID, userID := uuid.New(), uuid.New()

	token := signer.Sign(approvalID, userID, time.Now().Add(-time.Minute))
	if _, err := signer.Verify(token, approvalID); err != ErrActionTokenInvalid {
		t.Fatalf("Verify() error = %v, want ErrActionTokenInvalid", err)
	}
}

func TestActionTokenSigner_TamperedRejected(t *testing.T) {
	signer := NewActionTokenSigner(secret.Value("test-secret"))
	approvalID, userID := uuid.New(), uuid.New()

	token := signer.Sign(approvalID, userID, time.Now().Add(time.Hour))
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("token has unexpected shape: %q", token)
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	// Flip a full byte well inside the digest, not the string's last
	// character — RawURLEncoding's final base64 character for a 32-byte
	// SHA256 digest carries 2 padding bits alongside 4 real ones, so a
	// literal last-char string edit only actually changes the decoded
	// bytes ~75% of the time (this was a real, ~1-in-8-runs flaky test,
	// caught live via `make coverage`, not a hypothetical).
	sigBytes[0] ^= 0xFF
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(sigBytes)

	if _, err := signer.Verify(tampered, approvalID); err != ErrActionTokenInvalid {
		t.Fatalf("Verify() error = %v, want ErrActionTokenInvalid", err)
	}
}

func TestActionTokenSigner_WrongApprovalIDRejected(t *testing.T) {
	signer := NewActionTokenSigner(secret.Value("test-secret"))
	approvalA, approvalB, userID := uuid.New(), uuid.New(), uuid.New()

	token := signer.Sign(approvalA, userID, time.Now().Add(time.Hour))
	if _, err := signer.Verify(token, approvalB); err != ErrActionTokenInvalid {
		t.Fatalf("Verify() against a different approval id error = %v, want ErrActionTokenInvalid", err)
	}
}

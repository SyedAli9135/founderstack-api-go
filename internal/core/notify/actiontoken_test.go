package notify

import (
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
	tampered := token[:len(token)-1] + "x"
	if tampered == token {
		t.Fatal("tampered token equals original — test setup bug")
	}
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

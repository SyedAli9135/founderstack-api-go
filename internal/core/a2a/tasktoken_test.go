package a2a

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/founderstack/api/internal/pkg/secret"
)

func TestTaskTokenSigner_RoundTrip(t *testing.T) {
	signer := NewTaskTokenSigner(secret.Value("test-secret"))
	orgID, agentID, runID, taskID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	token := signer.Sign(orgID, agentID, runID, taskID, time.Now().Add(time.Hour))
	if token == "" {
		t.Fatal("Sign() returned empty token with a configured secret")
	}

	gotOrgID, err := signer.Verify(token, agentID, runID, taskID)
	if err != nil {
		t.Fatalf("Verify() error = %v, want nil", err)
	}
	if gotOrgID != orgID {
		t.Fatalf("Verify() orgID = %v, want %v", gotOrgID, orgID)
	}
}

func TestTaskTokenSigner_UnsetSecretRejectsEverything(t *testing.T) {
	signer := NewTaskTokenSigner(secret.Value(""))
	orgID, agentID, runID, taskID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	if token := signer.Sign(orgID, agentID, runID, taskID, time.Now().Add(time.Hour)); token != "" {
		t.Fatalf("Sign() with unset secret = %q, want empty", token)
	}

	signed := NewTaskTokenSigner(secret.Value("other-secret")).Sign(orgID, agentID, runID, taskID, time.Now().Add(time.Hour))
	if _, err := signer.Verify(signed, agentID, runID, taskID); err != ErrTaskTokenInvalid {
		t.Fatalf("Verify() error = %v, want ErrTaskTokenInvalid", err)
	}
}

func TestTaskTokenSigner_ExpiredRejected(t *testing.T) {
	signer := NewTaskTokenSigner(secret.Value("test-secret"))
	orgID, agentID, runID, taskID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	token := signer.Sign(orgID, agentID, runID, taskID, time.Now().Add(-time.Minute))
	if _, err := signer.Verify(token, agentID, runID, taskID); err != ErrTaskTokenInvalid {
		t.Fatalf("Verify() error = %v, want ErrTaskTokenInvalid", err)
	}
}

func TestTaskTokenSigner_TamperedRejected(t *testing.T) {
	signer := NewTaskTokenSigner(secret.Value("test-secret"))
	orgID, agentID, runID, taskID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	token := signer.Sign(orgID, agentID, runID, taskID, time.Now().Add(time.Hour))
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("token has unexpected shape: %q", token)
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	// Flip a full byte well inside the digest, not the string's last
	// character — see notify.ActionTokenSignerTest's identical note on why
	// a last-char edit is flaky for a RawURLEncoding-encoded SHA256 digest.
	sigBytes[0] ^= 0xFF
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(sigBytes)

	if _, err := signer.Verify(tampered, agentID, runID, taskID); err != ErrTaskTokenInvalid {
		t.Fatalf("Verify() error = %v, want ErrTaskTokenInvalid", err)
	}
}

// TestTaskTokenSigner_WrongAgentRejected is this token's whole point: it
// authorizes dispatch to exactly one agent, not "any agent this org owns"
// — a compromised/buggy caller can't reuse a token minted for the Finance
// agent to instead dispatch to (and consume the budget/tools of) some
// other agent entirely.
func TestTaskTokenSigner_WrongAgentRejected(t *testing.T) {
	signer := NewTaskTokenSigner(secret.Value("test-secret"))
	orgID, agentA, agentB, runID, taskID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()

	token := signer.Sign(orgID, agentA, runID, taskID, time.Now().Add(time.Hour))
	if _, err := signer.Verify(token, agentB, runID, taskID); err != ErrTaskTokenInvalid {
		t.Fatalf("Verify() against a different agent id error = %v, want ErrTaskTokenInvalid", err)
	}
}

// A token authorizes one specific delegation: this agent, for this
// dispatching run, creating this task id. Replaying it for any other run or
// task must fail even though the signature, org and agent all match.
func TestTaskTokenSigner_BoundToRunAndTask(t *testing.T) {
	signer := NewTaskTokenSigner(secret.Value("test-secret"))
	orgID, agentID, runID, taskID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	token := signer.Sign(orgID, agentID, runID, taskID, time.Now().Add(time.Hour))

	if _, err := signer.Verify(token, agentID, uuid.New(), taskID); err != ErrTaskTokenInvalid {
		t.Errorf("Verify() with another run id error = %v, want ErrTaskTokenInvalid", err)
	}
	if _, err := signer.Verify(token, agentID, runID, uuid.New()); err != ErrTaskTokenInvalid {
		t.Errorf("Verify() with another task id error = %v, want ErrTaskTokenInvalid", err)
	}
	if got, err := signer.Verify(token, agentID, runID, taskID); err != nil || got != orgID {
		t.Errorf("Verify() with the matching ids = (%v, %v), want (%v, nil)", got, err, orgID)
	}
}

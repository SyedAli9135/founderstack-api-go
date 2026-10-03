package a2a

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/founderstack/api/internal/pkg/secret"
)

var ErrTaskTokenInvalid = errors.New("a2a: invalid or expired task token")

// TaskTokenSigner mints/verifies the short-lived bearer token an
// orchestrator's delegate node attaches to its real HTTP POST
// .../a2a/agents/{agent_id}/tasks/send call — the only way that handler
// authenticates a request with no Clerk session behind it (this is a
// server-to-server call the harness itself makes, never a browser).
// Proves org_id + which specific agent this token authorizes dispatching
// to; the handler still re-checks IsAgentOnActiveTeam before running
// anything.
type TaskTokenSigner struct {
	secret secret.Value
}

func NewTaskTokenSigner(secret secret.Value) *TaskTokenSigner {
	return &TaskTokenSigner{secret: secret}
}

// Sign returns "" when the secret is unset, same convention as
// notify.ActionTokenSigner.Sign — a caller can tell at dispatch time
// whether team runs are actually configured, rather than sending a token
// Verify will always reject.
func (s *TaskTokenSigner) Sign(orgID, agentID, runID, taskID uuid.UUID, expiresAt time.Time) string {
	if s.secret.IsEmpty() {
		return ""
	}
	payload := taskTokenPayload(orgID, agentID, runID, taskID, expiresAt)
	mac := hmac.New(sha256.New, []byte(s.secret.Expose()))
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// Verify checks token's HMAC, expiry, and that it was minted for exactly
// this agentID (a token minted to dispatch to agent A is never valid
// against agent B, even from the same org/signer) — then returns the
// org_id it was minted for. The handler has no other way to learn which
// org a tasks/send request belongs to (there is no Clerk session on this
// call, see this package's doc comment), so orgID is an output here, not
// an input to check against — unlike agentID, which the handler already
// knows independently from the URL path and must confirm the token agrees
// with, not trust blindly.
func (s *TaskTokenSigner) Verify(token string, agentID, runID, taskID uuid.UUID) (orgID uuid.UUID, err error) {
	if s.secret.IsEmpty() {
		return uuid.Nil, ErrTaskTokenInvalid
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return uuid.Nil, ErrTaskTokenInvalid
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return uuid.Nil, ErrTaskTokenInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return uuid.Nil, ErrTaskTokenInvalid
	}

	mac := hmac.New(sha256.New, []byte(s.secret.Expose()))
	mac.Write(payloadBytes)
	expectedSig := mac.Sum(nil)
	if !hmac.Equal(sig, expectedSig) {
		return uuid.Nil, ErrTaskTokenInvalid
	}

	gotOrgID, gotAgentID, gotRunID, gotTaskID, expiresAt, err := parseTaskTokenPayload(string(payloadBytes))
	if err != nil {
		return uuid.Nil, ErrTaskTokenInvalid
	}
	// Bound to one agent, one dispatching run and one task id, so a leaked
	// token can't start any other specialist run.
	if subtle.ConstantTimeCompare(gotAgentID[:], agentID[:]) != 1 ||
		subtle.ConstantTimeCompare(gotRunID[:], runID[:]) != 1 ||
		subtle.ConstantTimeCompare(gotTaskID[:], taskID[:]) != 1 {
		return uuid.Nil, ErrTaskTokenInvalid
	}
	if time.Now().After(expiresAt) {
		return uuid.Nil, ErrTaskTokenInvalid
	}
	return gotOrgID, nil
}

func taskTokenPayload(orgID, agentID, runID, taskID uuid.UUID, expiresAt time.Time) string {
	return orgID.String() + "|" + agentID.String() + "|" + runID.String() + "|" + taskID.String() + "|" + strconv.FormatInt(expiresAt.Unix(), 10)
}

func parseTaskTokenPayload(payload string) (orgID, agentID, runID, taskID uuid.UUID, expiresAt time.Time, err error) {
	bad := func() (uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, time.Time, error) {
		return uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, time.Time{}, ErrTaskTokenInvalid
	}
	parts := strings.Split(payload, "|")
	if len(parts) != 5 {
		return bad()
	}
	ids := make([]uuid.UUID, 4)
	for i := range ids {
		if ids[i], err = uuid.Parse(parts[i]); err != nil {
			return bad()
		}
	}
	unixSeconds, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil {
		return bad()
	}
	return ids[0], ids[1], ids[2], ids[3], time.Unix(unixSeconds, 0), nil
}

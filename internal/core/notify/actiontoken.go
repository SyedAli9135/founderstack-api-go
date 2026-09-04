package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/founderstack/api/internal/pkg/secret"
)

// ErrActionTokenInvalid covers every rejection reason (unset secret,
// malformed token, tampered signature, expired, wrong approval) — a
// caller checking authorization never needs to distinguish which, only
// whether the token proves identity.
var ErrActionTokenInvalid = errors.New("notify: invalid or expired action token")

// Mints/verifies the single-purpose "magic link" token embedded in a
// push notification's payload — the only way a service worker's
// notificationclick handler can prove who's tapping without a live Clerk
// session. Proves identity only; the caller still re-checks
// can_approve_workflows/agents_paused before acting on it.
type ActionTokenSigner struct {
	secret secret.Value
}

func NewActionTokenSigner(secret secret.Value) *ActionTokenSigner {
	return &ActionTokenSigner{secret: secret}
}

// Empty when the secret is unset, so a caller can tell at send-time
// whether to embed a working action button — Verify always rejects an
// empty/unset-secret token, never "no secret configured" = "accept anything".
func (s *ActionTokenSigner) Sign(approvalID, userID uuid.UUID, expiresAt time.Time) string {
	if s.secret.IsEmpty() {
		return ""
	}
	payload := payloadFor(approvalID, userID, expiresAt)
	mac := hmac.New(sha256.New, []byte(s.secret.Expose()))
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// Verify checks token's HMAC, expiry, and that it was minted for
// approvalID specifically — a token for approval A is never valid
// against approval B, even from the same signer/secret.
func (s *ActionTokenSigner) Verify(token string, approvalID uuid.UUID) (userID uuid.UUID, err error) {
	if s.secret.IsEmpty() {
		return uuid.Nil, ErrActionTokenInvalid
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return uuid.Nil, ErrActionTokenInvalid
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return uuid.Nil, ErrActionTokenInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return uuid.Nil, ErrActionTokenInvalid
	}

	mac := hmac.New(sha256.New, []byte(s.secret.Expose()))
	mac.Write(payloadBytes)
	expectedSig := mac.Sum(nil)
	if !hmac.Equal(sig, expectedSig) {
		return uuid.Nil, ErrActionTokenInvalid
	}

	gotApprovalID, gotUserID, expiresAt, err := parsePayload(string(payloadBytes))
	if err != nil {
		return uuid.Nil, ErrActionTokenInvalid
	}
	if subtle.ConstantTimeCompare(gotApprovalID[:], approvalID[:]) != 1 {
		return uuid.Nil, ErrActionTokenInvalid
	}
	if time.Now().After(expiresAt) {
		return uuid.Nil, ErrActionTokenInvalid
	}
	return gotUserID, nil
}

func payloadFor(approvalID, userID uuid.UUID, expiresAt time.Time) string {
	return fmt.Sprintf("%s|%s|%d", approvalID, userID, expiresAt.Unix())
}

func parsePayload(payload string) (approvalID, userID uuid.UUID, expiresAt time.Time, err error) {
	parts := strings.Split(payload, "|")
	if len(parts) != 3 {
		return uuid.Nil, uuid.Nil, time.Time{}, ErrActionTokenInvalid
	}
	approvalID, err = uuid.Parse(parts[0])
	if err != nil {
		return uuid.Nil, uuid.Nil, time.Time{}, ErrActionTokenInvalid
	}
	userID, err = uuid.Parse(parts[1])
	if err != nil {
		return uuid.Nil, uuid.Nil, time.Time{}, ErrActionTokenInvalid
	}
	unixSeconds, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return uuid.Nil, uuid.Nil, time.Time{}, ErrActionTokenInvalid
	}
	return approvalID, userID, time.Unix(unixSeconds, 0), nil
}

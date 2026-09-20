package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/founderstack/api/internal/pkg/secret"
)

// ErrDigestTokenInvalid covers every rejection reason (unset secret,
// malformed token, tampered signature) -- a caller only ever needs to
// know whether the token proves the org, not why it didn't.
var ErrDigestTokenInvalid = errors.New("notify: invalid digest unsubscribe token")

// DigestTokenSigner mints/verifies the one-click unsubscribe link in a
// digest email's footer -- the only way a founder can turn the digest off
// without a live Clerk session, since the whole point is it arrives in an
// inbox. Org-scoped only, deliberately no expiry (unlike
// ActionTokenSigner's approval-scoped tokens): an unsubscribe link has no
// natural TTL, it should keep working for as long as digest emails could
// still arrive. Dedicated secret, not reused from PushActionTokenSecret
// or A2ATaskTokenSecret, for the same blast-radius-containment reasoning
// those two already follow.
type DigestTokenSigner struct {
	secret secret.Value
}

func NewDigestTokenSigner(secret secret.Value) *DigestTokenSigner {
	return &DigestTokenSigner{secret: secret}
}

// Sign returns "" when the secret is unset, so a caller can tell at
// send-time whether to embed a working unsubscribe link -- Verify always
// rejects an empty/unset-secret token, never treats "no secret
// configured" as "accept anything".
func (s *DigestTokenSigner) Sign(orgID uuid.UUID) string {
	if s.secret.IsEmpty() {
		return ""
	}
	payload := orgID[:]
	mac := hmac.New(sha256.New, []byte(s.secret.Expose()))
	mac.Write(payload)
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (s *DigestTokenSigner) Verify(token string) (orgID uuid.UUID, err error) {
	if s.secret.IsEmpty() {
		return uuid.Nil, ErrDigestTokenInvalid
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return uuid.Nil, ErrDigestTokenInvalid
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(payloadBytes) != 16 {
		return uuid.Nil, ErrDigestTokenInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return uuid.Nil, ErrDigestTokenInvalid
	}

	mac := hmac.New(sha256.New, []byte(s.secret.Expose()))
	mac.Write(payloadBytes)
	expectedSig := mac.Sum(nil)
	if !hmac.Equal(sig, expectedSig) {
		return uuid.Nil, ErrDigestTokenInvalid
	}

	var id uuid.UUID
	copy(id[:], payloadBytes)
	return id, nil
}

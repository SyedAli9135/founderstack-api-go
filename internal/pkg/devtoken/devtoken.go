// Package devtoken mints and verifies short-lived HS256 tokens for local
// manual testing without a real Clerk sign-in flow. This backend verifies
// real Clerk tokens against Clerk's JWKS (unlike founderstack-api's
// unsigned-JWT dev token, which only works because that backend's auth
// skips signature verification), so this is a genuinely separate,
// symmetric sign/verify path under one shared secret (DEV_TOKEN_SECRET),
// not a weakening of the real one.
package devtoken

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrInvalid covers every verification failure (bad signature, wrong
// secret, expired, malformed) — callers only need to know valid or not.
var ErrInvalid = errors.New("devtoken: invalid or expired token")

const ttl = 24 * time.Hour

// claims mirrors the one Clerk session-token field this backend reads
// beyond the subject: the active organization (Clerk's v1 "org_id").
type claims struct {
	jwt.RegisteredClaims
	OrgID string `json:"org_id,omitempty"`
}

// Sign mints a token asserting clerkUserID as the subject, with no active
// org. Callers must ensure secret (DEV_TOKEN_SECRET) is non-empty — Sign
// doesn't guard against an empty secret, since that's a config-validation
// concern.
func Sign(secret, clerkUserID string) (string, error) {
	return SignForOrg(secret, clerkUserID, "")
}

// SignForOrg is Sign plus an active-org claim (a clerk_org_id), the dev
// equivalent of a Clerk session after setActive({organization}).
func SignForOrg(secret, clerkUserID, clerkOrgID string) (string, error) {
	now := time.Now()
	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   clerkUserID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		OrgID: clerkOrgID,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	return token.SignedString([]byte(secret))
}

// Verify checks a token signed by Sign/SignForOrg under the same secret and
// returns its subject (a clerk_user_id) and active clerk_org_id ("" if none).
func Verify(secret, tokenString string) (clerkUserID, clerkOrgID string, err error) {
	var c claims
	token, err := jwt.ParseWithClaims(tokenString, &c, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalid
		}
		return []byte(secret), nil
	})
	if err != nil || !token.Valid {
		return "", "", ErrInvalid
	}
	return c.Subject, c.OrgID, nil
}

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

// Sign mints a token asserting clerkUserID as the subject. Callers must
// ensure secret (DEV_TOKEN_SECRET) is non-empty — Sign doesn't guard
// against an empty secret, since that's a config-validation concern.
func Sign(secret, clerkUserID string) (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Subject:   clerkUserID,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// Verify checks a token signed by Sign under the same secret and returns
// its subject (a clerk_user_id) on success.
func Verify(secret, tokenString string) (string, error) {
	var claims jwt.RegisteredClaims
	token, err := jwt.ParseWithClaims(tokenString, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalid
		}
		return []byte(secret), nil
	})
	if err != nil || !token.Valid {
		return "", ErrInvalid
	}
	return claims.Subject, nil
}

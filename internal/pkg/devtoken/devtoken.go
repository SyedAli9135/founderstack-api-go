// Package devtoken mints and verifies short-lived HS256 tokens for local
// manual testing (Postman etc.) without a real Clerk sign-in flow.
//
// This exists because founderstack-api's POST /api/v1/auth/dev-token works
// by minting an *unsigned* JWT, which only works there because that
// backend's auth explicitly skips signature verification
// (jwt.decode(token, options={"verify_signature": False})). This backend
// verifies real Clerk tokens for real (middleware.RequireAuth, against
// Clerk's JWKS), so a Python-style unsigned token would simply be
// rejected. This is a genuinely separate, parallel dev-only auth path —
// symmetric sign/verify under one shared secret (DEV_TOKEN_SECRET) — not
// a weakening of the real path. RequireAuth only attempts Verify as a
// fallback after real Clerk verification has already failed, and only
// when running outside production.
package devtoken

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrInvalid covers every verification failure (bad signature, wrong
// secret, expired, malformed) — deliberately not distinguished further,
// since callers only need "is this a valid dev token or not."
var ErrInvalid = errors.New("devtoken: invalid or expired token")

// ttl is short — this is a manual-testing convenience, not a real session;
// minting a new one is one API call away.
const ttl = 24 * time.Hour

// Sign mints a token asserting clerkUserID as the subject. secret is
// DEV_TOKEN_SECRET; callers must ensure it's non-empty before calling —
// Sign itself doesn't guard against an empty secret, since "produce a
// token signed with an empty key" is a config-validation concern for the
// caller, not something this function should silently allow or reject.
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

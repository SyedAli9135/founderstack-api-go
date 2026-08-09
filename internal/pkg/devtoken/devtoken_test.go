package devtoken

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestSignVerify_RoundTrip(t *testing.T) {
	token, err := Sign("dev-secret", "user_abc123")
	if err != nil {
		t.Fatalf("Sign() error = %v, want nil", err)
	}
	sub, err := Verify("dev-secret", token)
	if err != nil {
		t.Fatalf("Verify() error = %v, want nil", err)
	}
	if sub != "user_abc123" {
		t.Fatalf("Verify() = %q, want %q", sub, "user_abc123")
	}
}

func TestVerify_WrongSecretFails(t *testing.T) {
	token, err := Sign("dev-secret", "user_abc123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify("a-different-secret", token); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Verify() with wrong secret error = %v, want ErrInvalid", err)
	}
}

func TestVerify_GarbageTokenFails(t *testing.T) {
	if _, err := Verify("dev-secret", "not.a.jwt"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Verify() error = %v, want ErrInvalid", err)
	}
}

func TestVerify_ExpiredTokenFails(t *testing.T) {
	claims := jwt.RegisteredClaims{
		Subject:   "user_abc123",
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-48 * time.Hour)),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(-24 * time.Hour)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte("dev-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify("dev-secret", signed); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Verify() of expired token error = %v, want ErrInvalid", err)
	}
}

func TestVerify_RejectsAlgNoneAttack(t *testing.T) {
	// A classic JWT vulnerability: a token claiming alg=none (or any
	// non-HMAC algorithm) must never be accepted here, regardless of what
	// its signature (or lack of one) looks like.
	claims := jwt.RegisteredClaims{Subject: "user_attacker"}
	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	signed, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify("dev-secret", signed); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Verify() of an alg=none token error = %v, want ErrInvalid", err)
	}
}

package integrations

import (
	"context"
	"errors"
	"testing"
)

// These cover the paths that reject a state before ever touching Redis —
// Verify checks the HMAC signature first specifically so a garbage/forged
// state costs nothing but CPU, which is also what makes it safe to unit
// test with a nil *redis.Client here.
func TestStateManager_Verify_RejectsWithoutRedis(t *testing.T) {
	sm := NewStateManager(nil, "test-secret")
	ctx := context.Background()

	cases := []struct {
		name  string
		state string
	}{
		{"empty", ""},
		{"no separator", "just-a-nonce-no-dot"},
		{"empty nonce", ".somesig"},
		{"empty signature", "somenonce."},
		{"wrong signature", "somenonce.not-the-real-signature"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := sm.Verify(ctx, tc.state)
			if !errors.Is(err, ErrStateInvalid) {
				t.Fatalf("state %q: got err %v, want ErrStateInvalid", tc.state, err)
			}
		})
	}
}

func TestStateManager_Verify_ValidSignatureDifferentSecret(t *testing.T) {
	// A state minted under one secret must not verify under another —
	// simulates an OAUTH_STATE_SECRET rotation or a forged value signed
	// with a guessed/leaked-elsewhere secret.
	signed := NewStateManager(nil, "secret-a")
	nonce := "abc123"
	state := nonce + "." + signed.sign(nonce)

	verifier := NewStateManager(nil, "secret-b")
	if _, _, err := verifier.Verify(context.Background(), state); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("got err %v, want ErrStateInvalid", err)
	}
}

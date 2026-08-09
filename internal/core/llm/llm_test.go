package llm

import (
	"context"
	"errors"
	"testing"
)

// Only the two branches that never make a network call are covered here —
// ValidateKey's real-API path deliberately isn't part of the automated
// suite, the same reasoning as health.go's Pinecone/DB checks not being
// unit tested directly: a test suite that depends on a third-party
// service's live availability to pass is a flakiness and cost liability,
// not a correctness win. That path is exercised manually / via the BYOK
// endpoint's own manual verification instead.

func TestValidateKey_RejectsWrongFormat(t *testing.T) {
	tests := []string{
		"",
		"hunter2",
		"sk-openai-not-anthropic",
		"Bearer sk-ant-with-a-prefix-in-front",
	}
	for _, key := range tests {
		t.Run(key, func(t *testing.T) {
			err := ValidateKey(context.Background(), key, "sk-ant-test-")
			if !errors.Is(err, ErrInvalidFormat) {
				t.Errorf("ValidateKey(%q) error = %v, want ErrInvalidFormat", key, err)
			}
		})
	}
}

func TestValidateKey_MockPrefixShortCircuitsWithoutNetworkCall(t *testing.T) {
	err := ValidateKey(context.Background(), "sk-ant-test-anything-after-this", "sk-ant-test-")
	if err != nil {
		t.Errorf("ValidateKey() with mock prefix error = %v, want nil", err)
	}
}

func TestValidateKey_EmptyMockPrefixNeverMatches(t *testing.T) {
	// If ANTHROPIC_API_KEY_MOCK_PREFIX is unset (empty string), a key must
	// never mock-match against it — strings.HasPrefix(anything, "") is
	// true for every string, which would silently disable real validation
	// entirely if that env var were ever left blank. Uses the injectable
	// verify (see validateKey) so this is provable without a real
	// Anthropic API call: assert verify was actually invoked.
	var verifyCalled bool
	fakeVerify := func(_ context.Context, _ string) error {
		verifyCalled = true
		return nil
	}

	if err := validateKey(context.Background(), "sk-ant-not-the-mock-prefix", "", fakeVerify); err != nil {
		t.Fatalf("validateKey() error = %v, want nil (fakeVerify returns nil)", err)
	}
	if !verifyCalled {
		t.Fatal("verify was never called — an empty mockPrefix incorrectly short-circuited validation")
	}
}

func TestValidateKey_NonMockKeyGoesToVerify(t *testing.T) {
	var gotKey string
	fakeVerify := func(_ context.Context, apiKey string) error {
		gotKey = apiKey
		return nil
	}

	const key = "sk-ant-a-real-looking-key"
	if err := validateKey(context.Background(), key, "sk-ant-test-", fakeVerify); err != nil {
		t.Fatalf("validateKey() error = %v, want nil", err)
	}
	if gotKey != key {
		t.Fatalf("verify received %q, want %q", gotKey, key)
	}
}

func TestValidateKey_PropagatesVerifyError(t *testing.T) {
	wantErr := ErrKeyRejected
	fakeVerify := func(_ context.Context, _ string) error {
		return wantErr
	}

	err := validateKey(context.Background(), "sk-ant-whatever", "sk-ant-test-", fakeVerify)
	if !errors.Is(err, wantErr) {
		t.Fatalf("validateKey() error = %v, want %v", err, wantErr)
	}
}

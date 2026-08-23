package llm

import (
	"context"
	"errors"
	"testing"
)

// Only the branches that never make a network call are covered here —
// ValidateKey's real-API path deliberately isn't part of the automated
// suite for any of the 5 providers, the same reasoning as health.go's
// Pinecone/DB checks not being unit tested directly: a test suite that
// depends on a third-party service's live availability to pass is a
// flakiness and cost liability, not a correctness win. Each provider's
// request-shape logic (status codes, headers) is covered instead in
// verify_test.go via httptest fake servers, not live APIs.

func TestValidateKey_UnknownProviderRejected(t *testing.T) {
	err := ValidateKey(context.Background(), ProviderID("made-up-provider"), "anything", "mock-test-key-")
	if !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("ValidateKey() error = %v, want ErrUnknownProvider", err)
	}
}

func TestValidateKey_RejectsWrongFormatPerProvider(t *testing.T) {
	tests := []struct {
		provider ProviderID
		key      string
	}{
		{ProviderAnthropic, ""},
		{ProviderAnthropic, "hunter2"},
		{ProviderAnthropic, "sk-openai-not-anthropic"},
		{ProviderAnthropic, "Bearer sk-ant-with-a-prefix-in-front"},
		{ProviderOpenAI, "hunter2"},
		{ProviderOpenAI, "AIza-thats-a-gemini-key"},
		{ProviderGemini, "sk-not-a-gemini-key"},
		{ProviderQwen, "hunter2"},
		{ProviderDeepSeek, "hunter2"},
	}
	for _, tt := range tests {
		t.Run(string(tt.provider)+"/"+tt.key, func(t *testing.T) {
			err := ValidateKey(context.Background(), tt.provider, tt.key, "mock-test-key-")
			if !errors.Is(err, ErrInvalidFormat) {
				t.Errorf("ValidateKey(%q, %q) error = %v, want ErrInvalidFormat", tt.provider, tt.key, err)
			}
		})
	}
}

func TestValidateKey_MockPrefixShortCircuitsForEveryProvider(t *testing.T) {
	// Proves the verifiers/Catalog wiring for every provider without
	// touching the network: a mock-prefixed key short-circuits before
	// either the format check or the real verify runs, regardless of how
	// different that provider's real key format is (Gemini's "AIza" vs.
	// everyone else's "sk-").
	const mockPrefix = "mock-test-key-"
	for provider := range Catalog {
		t.Run(string(provider), func(t *testing.T) {
			err := ValidateKey(context.Background(), provider, mockPrefix+"anything-after-this", mockPrefix)
			if err != nil {
				t.Errorf("ValidateKey(%q) with mock prefix error = %v, want nil", provider, err)
			}
		})
	}
}

func TestValidateKey_EmptyMockPrefixNeverMatches(t *testing.T) {
	// If API_KEY_MOCK_PREFIX is unset (empty string), a key must never
	// mock-match against it — strings.HasPrefix(anything, "") is true for
	// every string, which would silently disable real validation entirely
	// if that env var were ever left blank. Uses the injectable verify
	// (see validateKey) so this is provable without a real API call:
	// assert verify was actually invoked.
	var verifyCalled bool
	fakeVerify := func(_ context.Context, _ string) error {
		verifyCalled = true
		return nil
	}

	if err := validateKey(context.Background(), "sk-ant-not-the-mock-prefix", "sk-ant-", "", fakeVerify); err != nil {
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
	if err := validateKey(context.Background(), key, "sk-ant-", "mock-test-key-", fakeVerify); err != nil {
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

	err := validateKey(context.Background(), "sk-ant-whatever", "sk-ant-", "mock-test-key-", fakeVerify)
	if !errors.Is(err, wantErr) {
		t.Fatalf("validateKey() error = %v, want %v", err, wantErr)
	}
}

func TestCatalog_HasAllFiveProvidersWithAVerifier(t *testing.T) {
	want := []ProviderID{ProviderAnthropic, ProviderOpenAI, ProviderGemini, ProviderQwen, ProviderDeepSeek}
	if len(Catalog) != len(want) {
		t.Fatalf("len(Catalog) = %d, want %d", len(Catalog), len(want))
	}
	for _, p := range want {
		meta, ok := Catalog[p]
		if !ok {
			t.Errorf("Catalog missing provider %q", p)
			continue
		}
		if meta.Name == "" {
			t.Errorf("Catalog[%q].Name is empty", p)
		}
		if _, ok := verifiers[p]; !ok {
			t.Errorf("verifiers missing provider %q — every Catalog entry needs a dispatchable verify", p)
		}
	}
}

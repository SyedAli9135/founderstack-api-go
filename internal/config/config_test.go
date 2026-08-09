package config

import (
	"strings"
	"testing"
)

// requiredEnvKeys mirrors requiredFields' keys, used to deliberately blank
// every required var before a "missing" test — relying on them being
// merely "unset" would make the test flaky against whatever the ambient
// shell environment happens to export.
func requiredEnvKeys() []string {
	keys := make([]string, len(requiredFields))
	for i, f := range requiredFields {
		keys[i] = f.key
	}
	return keys
}

func setAllRequired(t *testing.T) {
	t.Helper()
	for _, key := range requiredEnvKeys() {
		t.Setenv(key, "value-for-"+key)
	}
}

func blankAllRequired(t *testing.T) {
	t.Helper()
	for _, key := range requiredEnvKeys() {
		t.Setenv(key, "")
	}
}

func TestLoad_SucceedsWithAllRequiredSet(t *testing.T) {
	setAllRequired(t)
	t.Setenv("DATABASE_URL", "postgresql://user:pass@localhost:5432/db")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.DatabaseURL != "postgresql://user:pass@localhost:5432/db" {
		t.Errorf("DatabaseURL = %q, want the value we set", cfg.DatabaseURL)
	}
	// Defaults for fields we never touched.
	if cfg.AppEnv != "development" {
		t.Errorf("AppEnv = %q, want default \"development\"", cfg.AppEnv)
	}
	if cfg.PineconeIndexRAG != "founderstack-rag" {
		t.Errorf("PineconeIndexRAG = %q, want default \"founderstack-rag\"", cfg.PineconeIndexRAG)
	}
	if cfg.DatabasePoolSize != 20 {
		t.Errorf("DatabasePoolSize = %d, want default 20", cfg.DatabasePoolSize)
	}
}

func TestLoad_ReportsEveryMissingRequiredField(t *testing.T) {
	blankAllRequired(t)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error listing all missing required fields")
	}
	for _, key := range requiredEnvKeys() {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not mention missing field %q", err.Error(), key)
		}
	}
}

func TestLoad_ReportsOnlyTheFieldsActuallyMissing(t *testing.T) {
	blankAllRequired(t)
	// Fill in every required field except one.
	present := requiredEnvKeys()
	missingKey := present[0]
	for _, key := range present[1:] {
		t.Setenv(key, "value-for-"+key)
	}

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error naming the one missing field")
	}
	if !strings.Contains(err.Error(), missingKey) {
		t.Errorf("error %q does not mention the missing field %q", err.Error(), missingKey)
	}
	for _, key := range present[1:] {
		if strings.Contains(err.Error(), key) {
			t.Errorf("error %q wrongly mentions %q, which was set", err.Error(), key)
		}
	}
}

func TestLoad_SecretFieldsAreWrappedNotPlainStrings(t *testing.T) {
	setAllRequired(t)
	t.Setenv("CLERK_SECRET_KEY", "sk_test_super_secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.ClerkSecretKey.Expose() != "sk_test_super_secret" {
		t.Errorf("ClerkSecretKey.Expose() = %q, want the configured value", cfg.ClerkSecretKey.Expose())
	}
	if cfg.ClerkSecretKey.String() == "sk_test_super_secret" {
		t.Error("ClerkSecretKey.String() leaked the plaintext secret")
	}
}

func TestIsProduction(t *testing.T) {
	tests := []struct {
		appEnv string
		want   bool
	}{
		{"production", true},
		{"development", false},
		{"staging", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.appEnv, func(t *testing.T) {
			cfg := &Config{AppEnv: tc.appEnv}
			if got := cfg.IsProduction(); got != tc.want {
				t.Errorf("IsProduction() with AppEnv=%q = %v, want %v", tc.appEnv, got, tc.want)
			}
		})
	}
}

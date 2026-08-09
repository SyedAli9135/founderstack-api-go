package main

import (
	"testing"

	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/pkg/secret"
)

func TestAddr(t *testing.T) {
	t.Run("defaults to 8000", func(t *testing.T) {
		t.Setenv("PORT", "")
		if got := addr(); got != ":8000" {
			t.Errorf("addr() = %q, want \":8000\"", got)
		}
	})
	t.Run("honors PORT", func(t *testing.T) {
		t.Setenv("PORT", "9090")
		if got := addr(); got != ":9090" {
			t.Errorf("addr() = %q, want \":9090\"", got)
		}
	})
}

func TestCorsConfig_Production(t *testing.T) {
	cfg := &config.Config{AppEnv: "production", AppBaseURL: "https://api.founderstack.ai"}
	c := corsConfig(cfg)

	if c.AllowOriginFunc != nil {
		t.Error("production CORS config should not use AllowOriginFunc — it must be a fixed allowlist")
	}
	found := false
	for _, o := range c.AllowOrigins {
		if o == "https://api.founderstack.ai" {
			found = true
		}
	}
	if !found {
		t.Errorf("AllowOrigins = %v, want it to include the configured APP_BASE_URL", c.AllowOrigins)
	}
	if !c.AllowCredentials {
		t.Error("AllowCredentials = false, want true")
	}
}

func TestCorsConfig_Development(t *testing.T) {
	cfg := &config.Config{AppEnv: "development"}
	c := corsConfig(cfg)

	// Development must NOT use a literal "*" AllowOrigins alongside
	// AllowCredentials — the CORS spec forbids that combination outright.
	// AllowOriginFunc is the correct way to be permissive in dev.
	if c.AllowOriginFunc == nil {
		t.Fatal("development CORS config should use AllowOriginFunc")
	}
	if !c.AllowOriginFunc("http://anything.example.com") {
		t.Error("development AllowOriginFunc should accept any origin")
	}
	for _, o := range c.AllowOrigins {
		if o == "*" {
			t.Error("AllowOrigins contains a literal \"*\" combined with AllowCredentials — browsers reject this")
		}
	}
}

func TestNewRedisClient_DefaultsToLocalhost(t *testing.T) {
	cfg := &config.Config{}
	client, err := newRedisClient(cfg)
	if err != nil {
		t.Fatalf("newRedisClient() error = %v, want nil", err)
	}
	defer client.Close()
	if got := client.Options().Addr; got != "localhost:6379" {
		t.Errorf("Addr = %q, want \"localhost:6379\"", got)
	}
}

func TestNewRedisClient_UsesConfiguredURLAndToken(t *testing.T) {
	cfg := &config.Config{
		UpstashRedisURL:   "redis://myhost:6380",
		UpstashRedisToken: secret.Value("s3cr3t"),
	}
	client, err := newRedisClient(cfg)
	if err != nil {
		t.Fatalf("newRedisClient() error = %v, want nil", err)
	}
	defer client.Close()
	if got := client.Options().Addr; got != "myhost:6380" {
		t.Errorf("Addr = %q, want \"myhost:6380\"", got)
	}
	if got := client.Options().Password; got != "s3cr3t" {
		t.Errorf("Password = %q, want \"s3cr3t\"", got)
	}
}

func TestNewRedisClient_RejectsInvalidURL(t *testing.T) {
	cfg := &config.Config{UpstashRedisURL: "not-a-valid-url::://"}
	if _, err := newRedisClient(cfg); err == nil {
		t.Fatal("newRedisClient() error = nil, want an error for an unparseable URL")
	}
}

func TestNewPineconeClient_NilWhenNoKeyConfigured(t *testing.T) {
	cfg := &config.Config{}
	client, err := newPineconeClient(cfg)
	if err != nil {
		t.Fatalf("newPineconeClient() error = %v, want nil", err)
	}
	if client != nil {
		t.Error("newPineconeClient() with no API key should return a nil client, not an error")
	}
}

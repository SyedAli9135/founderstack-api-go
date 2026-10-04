package middleware

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"

	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/pkg/devtoken"
)

func TestBearerToken(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"well-formed", "Bearer abc.def.ghi", "abc.def.ghi"},
		{"missing prefix", "abc.def.ghi", ""},
		{"empty header", "", ""},
		{"wrong scheme", "Basic dXNlcjpwYXNz", ""},
		{"prefix but no token", "Bearer ", ""},
		{"case-sensitive prefix", "bearer abc.def.ghi", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := bearerToken(tc.header); got != tc.want {
				t.Errorf("bearerToken(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

func TestJWKCache_CachesAfterFirstFetch(t *testing.T) {
	cache := NewJWKCache()
	calls := 0
	fetch := func(_ context.Context, keyID string) (*clerk.JSONWebKey, error) {
		calls++
		return &clerk.JSONWebKey{KeyID: keyID}, nil
	}

	for i := 0; i < 3; i++ {
		jwk, err := cache.get(context.Background(), "kid-1", fetch)
		if err != nil {
			t.Fatalf("get() error = %v, want nil", err)
		}
		if jwk.KeyID != "kid-1" {
			t.Fatalf("KeyID = %q, want \"kid-1\"", jwk.KeyID)
		}
	}
	if calls != 1 {
		t.Fatalf("fetch called %d times, want 1 (subsequent calls should hit the cache)", calls)
	}
}

func TestJWKCache_FetchesSeparatelyPerKeyID(t *testing.T) {
	cache := NewJWKCache()
	calls := 0
	fetch := func(_ context.Context, keyID string) (*clerk.JSONWebKey, error) {
		calls++
		return &clerk.JSONWebKey{KeyID: keyID}, nil
	}

	if _, err := cache.get(context.Background(), "kid-1", fetch); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.get(context.Background(), "kid-2", fetch); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("fetch called %d times, want 2 (different key IDs, e.g. after Clerk key rotation, must not share a cache slot)", calls)
	}
}

func TestJWKCache_DoesNotCacheOnFetchError(t *testing.T) {
	cache := NewJWKCache()
	wantErr := errors.New("clerk API unreachable")
	calls := 0
	fetch := func(_ context.Context, keyID string) (*clerk.JSONWebKey, error) {
		calls++
		return nil, wantErr
	}

	if _, err := cache.get(context.Background(), "kid-1", fetch); !errors.Is(err, wantErr) {
		t.Fatalf("get() error = %v, want %v", err, wantErr)
	}
	// A transient fetch failure must not poison the cache — retry on the
	// next request rather than permanently failing for this kid.
	if _, err := cache.get(context.Background(), "kid-1", fetch); !errors.Is(err, wantErr) {
		t.Fatalf("get() error = %v, want %v", err, wantErr)
	}
	if calls != 2 {
		t.Fatalf("fetch called %d times, want 2 (a failed fetch should not be cached)", calls)
	}
}

func TestDevTokenFallback_DisabledInProduction(t *testing.T) {
	cfg := &config.Config{AppEnv: "production", DevTokenSecret: "some-secret"}
	token, _ := devtoken.Sign("some-secret", "user_x")

	if _, _, err := devTokenFallback(cfg, token); err == nil {
		t.Fatal("devTokenFallback() succeeded in production, want it disabled regardless of a valid token")
	}
}

func TestDevTokenFallback_DisabledWhenSecretUnset(t *testing.T) {
	cfg := &config.Config{AppEnv: "development", DevTokenSecret: ""}
	token, _ := devtoken.Sign("anything", "user_x")

	if _, _, err := devTokenFallback(cfg, token); err == nil {
		t.Fatal("devTokenFallback() succeeded with an empty DevTokenSecret, want it disabled")
	}
}

func TestDevTokenFallback_EnabledInDevReturnsSubject(t *testing.T) {
	cfg := &config.Config{AppEnv: "development", DevTokenSecret: "dev-secret"}
	token, err := devtoken.Sign("dev-secret", "user_abc")
	if err != nil {
		t.Fatal(err)
	}

	sub, _, err := devTokenFallback(cfg, token)
	if err != nil {
		t.Fatalf("devTokenFallback() error = %v, want nil", err)
	}
	if sub != "user_abc" {
		t.Fatalf("devTokenFallback() = %q, want %q", sub, "user_abc")
	}
}

func TestJWKCache_RemembersDefinitiveUnknownKeyID(t *testing.T) {
	cache := NewJWKCache()
	calls := 0
	fetch := func(_ context.Context, keyID string) (*clerk.JSONWebKey, error) {
		calls++
		return nil, &clerk.APIErrorResponse{HTTPStatusCode: http.StatusNotFound}
	}

	for range 3 {
		if _, err := cache.get(context.Background(), "random-kid", fetch); err == nil {
			t.Fatal("get() error = nil, want an error for an unknown kid")
		}
	}
	if calls != 1 {
		t.Fatalf("fetch called %d times, want 1 (an unknown kid should be negatively cached)", calls)
	}
}

func TestCheckAuthorizedParty(t *testing.T) {
	allowed := []string{"https://app.founderstack.ai"}
	for _, tc := range []struct {
		name    string
		azp     string
		allowed []string
		wantErr bool
	}{
		{"matching origin", "https://app.founderstack.ai", allowed, false},
		{"another origin", "https://evil.example", allowed, true},
		{"a look-alike prefix", "https://app.founderstack.ai.evil.example", allowed, true},
		{"no azp claim", "", allowed, false},
		{"no list configured", "https://anything.example", nil, false},
	} {
		if err := checkAuthorizedParty(tc.azp, tc.allowed); (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
	}
}

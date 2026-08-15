//go:build integration

package integrations

import (
	"context"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis not reachable at localhost:6379 (run make docker-up): %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestStateManager_GenerateVerify_RoundTrip(t *testing.T) {
	rdb := newTestRedis(t)
	sm := NewStateManager(rdb, "integration-test-secret")
	ctx := context.Background()

	state, err := sm.Generate(ctx, "org-123", "slack")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	orgID, service, err := sm.Verify(ctx, state)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if orgID != "org-123" || service != "slack" {
		t.Fatalf("got (%q, %q), want (org-123, slack)", orgID, service)
	}
}

func TestStateManager_Verify_OneTimeUse(t *testing.T) {
	rdb := newTestRedis(t)
	sm := NewStateManager(rdb, "integration-test-secret")
	ctx := context.Background()

	state, err := sm.Generate(ctx, "org-123", "notion")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if _, _, err := sm.Verify(ctx, state); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	// Replaying the same state (e.g. a retried/duplicated callback) must
	// fail, not silently succeed a second time.
	if _, _, err := sm.Verify(ctx, state); !errors.Is(err, ErrStateExpired) {
		t.Fatalf("second Verify: got err %v, want ErrStateExpired", err)
	}
}

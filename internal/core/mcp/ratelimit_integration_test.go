//go:build integration

package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"
)

// newTestRedis matches internal/core/integrations/state_integration_test.go's
// own helper — same local Redis, same skip-if-unreachable behavior.
func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis not reachable at localhost:6379 (run make docker-up): %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestCheckRateLimit_AllowsUnderLimit(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	orgID := "org-" + t.Name()

	for i := 0; i < rateLimitMaxCalls; i++ {
		if err := checkRateLimit(ctx, rdb, orgID, "stripe"); err != nil {
			t.Fatalf("call %d: checkRateLimit() error = %v, want nil (under the %d-call limit)", i+1, err, rateLimitMaxCalls)
		}
	}
}

func TestCheckRateLimit_TripsOverLimitAndClassifiesRetryable(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	orgID := "org-" + t.Name()

	for i := 0; i < rateLimitMaxCalls; i++ {
		if err := checkRateLimit(ctx, rdb, orgID, "stripe"); err != nil {
			t.Fatalf("call %d within limit unexpectedly failed: %v", i+1, err)
		}
	}
	err := checkRateLimit(ctx, rdb, orgID, "stripe")
	if !errors.Is(err, ErrToolRetryable) {
		t.Fatalf("call over limit: err = %v, want ErrToolRetryable", err)
	}
}

func TestCheckRateLimit_ScopedPerOrgAndService(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	orgA, orgB := "org-a-"+t.Name(), "org-b-"+t.Name()

	// Exhaust orgA's stripe budget.
	for i := 0; i < rateLimitMaxCalls; i++ {
		if err := checkRateLimit(ctx, rdb, orgA, "stripe"); err != nil {
			t.Fatalf("orgA call %d unexpectedly failed: %v", i+1, err)
		}
	}
	if err := checkRateLimit(ctx, rdb, orgA, "stripe"); !errors.Is(err, ErrToolRetryable) {
		t.Fatalf("orgA should be over limit now: err = %v", err)
	}

	// A different org, and a different service on the same org, must
	// both be unaffected — the limit is per-(org_id, service), not
	// global or per-org-wide.
	if err := checkRateLimit(ctx, rdb, orgB, "stripe"); err != nil {
		t.Fatalf("orgB (different org) should be unaffected: %v", err)
	}
	if err := checkRateLimit(ctx, rdb, orgA, "slack"); err != nil {
		t.Fatalf("orgA's slack budget (different service) should be unaffected: %v", err)
	}
}

func TestCheckRateLimit_NilClientFailsOpen(t *testing.T) {
	if err := checkRateLimit(context.Background(), nil, "org-x", "stripe"); err != nil {
		t.Fatalf("checkRateLimit() with a nil client error = %v, want nil (fail open)", err)
	}
}

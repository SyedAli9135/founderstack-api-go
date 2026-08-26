package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	rateLimitWindow = time.Minute
	// rateLimitMaxCalls is a per-(org_id, service) ceiling, not a
	// per-org-wide one — a runaway loop hammering Stripe shouldn't also
	// starve that same org's Slack calls. 30/min is generous for normal
	// agent behavior (workflow 9's own max_tool_calls default is 25 for
	// an entire run) while still catching a real pathological loop.
	rateLimitMaxCalls = 30
)

// checkRateLimit enforces the per-(org_id, service) ceiling via a simple
// fixed-window INCR+EXPIRE — same Redis client and pattern already
// established by internal/core/integrations/state.go, no new
// infrastructure. A tripped limit is ErrToolRetryable, not terminal: the
// caller should back off and try again shortly, not treat this as "this
// tool call is broken."
//
// Fails open, not closed, on a Redis error — a rate limiter is a
// resource-protection measure, not a security guardrail (unlike the
// risk-tier/policy_scope checks, which fail closed by design); an
// unrelated Redis outage should not halt every tool call across every
// org.
func checkRateLimit(ctx context.Context, rdb *redis.Client, orgID, service string) error {
	if rdb == nil {
		return nil
	}
	key := fmt.Sprintf("ratelimit:tool:%s:%s", orgID, service)
	count, err := rdb.Incr(ctx, key).Result()
	if err != nil {
		return nil
	}
	if count == 1 {
		rdb.Expire(ctx, key, rateLimitWindow)
	}
	if count > rateLimitMaxCalls {
		return fmt.Errorf("%w: org %s exceeded %d %s calls per %s", ErrToolRetryable, orgID, rateLimitMaxCalls, service, rateLimitWindow)
	}
	return nil
}

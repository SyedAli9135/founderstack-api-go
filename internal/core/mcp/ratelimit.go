package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	rateLimitWindow   = time.Minute
	rateLimitMaxCalls = 30 // per (org_id, service), not org-wide
)

// checkRateLimit enforces the per-(org_id, service) ceiling via a
// fixed-window INCR+EXPIRE. Fails open (nil rdb or a Redis error both
// return nil, not an error) — this protects resources, it isn't a
// security guardrail, so a Redis outage shouldn't block every tool call.
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

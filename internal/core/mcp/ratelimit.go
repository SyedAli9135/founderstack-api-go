package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	rateLimitWindow = time.Minute
	// rateLimitMaxCalls is a per-(org_id, service) ceiling, not a per-org-wide one
	rateLimitMaxCalls = 30
)

// checkRateLimit enforces the per-(org_id, service) ceiling via a simple
// fixed-window INCR+EXPIRE — same Redis client and pattern already established "
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

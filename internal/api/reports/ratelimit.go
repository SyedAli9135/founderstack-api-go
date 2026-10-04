package reports

import (
	"context"

	"github.com/founderstack/api/internal/pkg/ratelimit"
)

// RateLimiter guards the one unauthenticated endpoint in this package.
type RateLimiter interface {
	Allow(ctx context.Context, key string) bool
}

// RedisLimiter fails open on a Redis error: the share token is 192 random
// bits, so the limit is defence in depth.
type RedisLimiter = ratelimit.Limiter

var NewRedisLimiter = ratelimit.New

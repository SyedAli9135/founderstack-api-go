package reports

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// RateLimiter guards the one unauthenticated endpoint in this package.
type RateLimiter interface {
	Allow(ctx context.Context, key string) bool
}

// RedisLimiter is a fixed-window counter in Redis, so the limit holds across
// several API processes. It fails open on a Redis error: the share token is
// 192 random bits, so the limit is defence in depth, and a Redis blip
// shouldn't take every client's report offline.
type RedisLimiter struct {
	rdb    *redis.Client
	limit  int64
	window time.Duration
}

func NewRedisLimiter(rdb *redis.Client, limit int64, window time.Duration) *RedisLimiter {
	return &RedisLimiter{rdb: rdb, limit: limit, window: window}
}

func (l *RedisLimiter) Allow(ctx context.Context, key string) bool {
	bucket := time.Now().UnixNano() / int64(l.window)
	k := fmt.Sprintf("ratelimit:%s:%d", key, bucket)
	pipe := l.rdb.TxPipeline()
	incr := pipe.Incr(ctx, k)
	pipe.Expire(ctx, k, l.window+time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Warn("reports: rate limiter unavailable, allowing request", "error", err)
		return true
	}
	return incr.Val() <= l.limit
}

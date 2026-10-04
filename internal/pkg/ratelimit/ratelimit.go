// Package ratelimit is a fixed-window counter in Redis, so a limit holds across
// every API process. It fails open on a Redis error: these limits protect cost
// and abuse rather than correctness, and a Redis blip shouldn't take features
// offline for everyone.
package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

type Limiter struct {
	rdb    *redis.Client
	limit  int64
	window time.Duration
}

// New builds a Limiter allowing limit units per key per window. A nil client
// means "no Redis configured": everything is allowed.
func New(rdb *redis.Client, limit int64, window time.Duration) *Limiter {
	return &Limiter{rdb: rdb, limit: limit, window: window}
}

// Allow counts one unit against key.
func (l *Limiter) Allow(ctx context.Context, key string) bool { return l.AllowN(ctx, key, 1) }

// AllowN counts n units (e.g. bytes) against key and reports whether the
// window's total is still within the limit.
func (l *Limiter) AllowN(ctx context.Context, key string, n int64) bool {
	if l == nil || l.rdb == nil {
		return true
	}
	bucket := time.Now().UnixNano() / int64(l.window)
	k := fmt.Sprintf("ratelimit:%s:%d", key, bucket)
	pipe := l.rdb.TxPipeline()
	incr := pipe.IncrBy(ctx, k, n)
	pipe.Expire(ctx, k, l.window+time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Warn("ratelimit: Redis unavailable, allowing request", "key", key, "error", err)
		return true
	}
	return incr.Val() <= l.limit
}

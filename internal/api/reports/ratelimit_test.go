package reports

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRedisLimiter_FailsOpenWhenRedisIsDown(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = rdb.Close() })
	l := NewRedisLimiter(rdb, 1, time.Minute)

	for i := 0; i < 3; i++ {
		if !l.Allow(context.Background(), "public-report:203.0.113.9") {
			t.Fatalf("request %d blocked; a Redis outage must not take shared reports offline", i+1)
		}
	}
}

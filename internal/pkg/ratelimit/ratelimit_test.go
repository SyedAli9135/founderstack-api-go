package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestLimiter_NilClientAllowsEverything(t *testing.T) {
	l := New(nil, 1, time.Minute)
	for i := 0; i < 5; i++ {
		if !l.Allow(context.Background(), "k") {
			t.Fatal("a limiter without Redis must allow")
		}
	}
	var none *Limiter
	if !none.Allow(context.Background(), "k") {
		t.Fatal("a nil limiter must allow")
	}
}

func TestLimiter_FailsOpenWhenRedisIsDown(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = rdb.Close() })
	if !New(rdb, 1, time.Minute).AllowN(context.Background(), "k", 5) {
		t.Fatal("a Redis outage must not block requests")
	}
}

func TestLimiter_CountsUnitsPerKeyAndWindow(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skip("redis not reachable: ", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	// A window length unique to this run keeps its keys apart from any other.
	l := New(rdb, 10, time.Hour+time.Duration(time.Now().UnixNano()%1000)*time.Millisecond)
	ctx := context.Background()

	if !l.AllowN(ctx, "a", 6) || !l.AllowN(ctx, "a", 4) {
		t.Fatal("10 units within a limit of 10 should pass")
	}
	if l.AllowN(ctx, "a", 1) {
		t.Fatal("the 11th unit should be refused")
	}
	if !l.AllowN(ctx, "b", 10) {
		t.Fatal("another key has its own count")
	}
}

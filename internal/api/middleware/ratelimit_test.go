package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/founderstack/api/internal/api/authctx"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skip("redis not reachable: ", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func serve(r *gin.Engine, method, path, remote string) int {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remote + ":1234"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code
}

func TestRateLimitIP_LimitsPerIPAndSkipsWebhooks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rdb := testRedis(t)
	// A bucket name unique to this run keeps its counters apart from any other.
	bucket := "test-" + uuid.NewString()
	r := gin.New()
	_ = r.SetTrustedProxies(nil)
	r.Use(RateLimitIP(rdb, bucket, 2, time.Hour, "/api/webhooks"))
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	r.GET("/x", ok)
	r.POST("/api/webhooks/clerk", ok)

	if a, b := serve(r, "GET", "/x", "203.0.113.1"), serve(r, "GET", "/x", "203.0.113.1"); a != 200 || b != 200 {
		t.Fatalf("first two = %d, %d, want 200", a, b)
	}
	if c := serve(r, "GET", "/x", "203.0.113.1"); c != http.StatusTooManyRequests {
		t.Fatalf("third = %d, want 429", c)
	}
	if c := serve(r, "GET", "/x", "203.0.113.2"); c != 200 {
		t.Fatalf("another IP = %d, want 200", c)
	}
	for i := 0; i < 5; i++ {
		if c := serve(r, "POST", "/api/webhooks/clerk", "203.0.113.1"); c != 200 {
			t.Fatalf("webhook request %d = %d, want 200 (exempt)", i, c)
		}
	}
}

func TestRateLimitRoutes_OnlyTheNamedRouteAndPerUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rdb := testRedis(t)
	path := "/limited-" + uuid.NewString()
	r := gin.New()
	var current uuid.UUID
	r.Use(func(c *gin.Context) {
		authctx.Set(c, authctx.User{ID: pgtype.UUID{Bytes: current, Valid: true}})
		c.Next()
	})
	r.Use(RateLimitRoutes(rdb, []RouteRule{{Method: "POST", Path: path, Limit: 1, Window: time.Hour}}))
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	r.POST(path, ok)
	r.GET(path, ok)

	current = uuid.New()
	if a, b := serve(r, "POST", path, "1.1.1.1"), serve(r, "POST", path, "1.1.1.1"); a != 200 || b != http.StatusTooManyRequests {
		t.Fatalf("POST twice = %d, %d, want 200 then 429", a, b)
	}
	if c := serve(r, "GET", path, "1.1.1.1"); c != 200 {
		t.Fatalf("GET on the same path = %d, want 200 (no rule)", c)
	}
	current = uuid.New()
	if c := serve(r, "POST", path, "1.1.1.1"); c != 200 {
		t.Fatalf("a different user = %d, want 200 (limits are per user)", c)
	}
}

package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/pkg/ratelimit"
)

func tooManyRequests(c *gin.Context, window time.Duration) {
	c.Header("Retry-After", strconv.Itoa(int(window.Seconds())))
	response.Fail(c, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests — slow down and try again shortly")
	c.Abort()
}

// RateLimitIP limits requests per client IP. Paths starting with any skip
// prefix are exempt (the Clerk and Stripe webhooks arrive from a few senders
// in bursts and are authenticated by signature). The client IP is only
// trustworthy when TRUSTED_PROXIES matches the real load balancer.
func RateLimitIP(rdb *redis.Client, bucket string, limit int64, window time.Duration, skipPrefixes ...string) gin.HandlerFunc {
	l := ratelimit.New(rdb, limit, window)
	return func(c *gin.Context) {
		for _, p := range skipPrefixes {
			if strings.HasPrefix(c.Request.URL.Path, p) {
				c.Next()
				return
			}
		}
		if !l.Allow(c.Request.Context(), bucket+":ip:"+c.ClientIP()) {
			tooManyRequests(c, window)
			return
		}
		c.Next()
	}
}

// RateLimitUser limits an authenticated caller; mount it after RequireAuth.
func RateLimitUser(rdb *redis.Client, bucket string, limit int64, window time.Duration) gin.HandlerFunc {
	l := ratelimit.New(rdb, limit, window)
	return func(c *gin.Context) {
		if user, ok := authctx.FromContext(c); ok {
			if !l.Allow(c.Request.Context(), bucket+":user:"+user.ID.String()) {
				tooManyRequests(c, window)
				return
			}
		}
		c.Next()
	}
}

// RouteRule is a tighter limit on one expensive route, counted per user.
type RouteRule struct {
	Method string
	// Path is the route pattern as registered, e.g. "/api/v1/reports".
	Path   string
	Limit  int64
	Window time.Duration
}

// RateLimitRoutes applies each rule to its route, per user. Mount it after
// RequireAuth. A route with no rule is untouched.
func RateLimitRoutes(rdb *redis.Client, rules []RouteRule) gin.HandlerFunc {
	type entry struct {
		limiter *ratelimit.Limiter
		window  time.Duration
	}
	byRoute := make(map[string]entry, len(rules))
	for _, r := range rules {
		byRoute[r.Method+" "+r.Path] = entry{ratelimit.New(rdb, r.Limit, r.Window), r.Window}
	}
	return func(c *gin.Context) {
		e, ok := byRoute[c.Request.Method+" "+c.FullPath()]
		if !ok {
			c.Next()
			return
		}
		if user, ok := authctx.FromContext(c); ok {
			if !e.limiter.Allow(c.Request.Context(), "route:"+c.Request.Method+c.FullPath()+":user:"+user.ID.String()) {
				tooManyRequests(c, e.window)
				return
			}
		}
		c.Next()
	}
}

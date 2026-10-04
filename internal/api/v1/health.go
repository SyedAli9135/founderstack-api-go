package v1

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pinecone-io/go-pinecone/v5/pinecone"
	"github.com/redis/go-redis/v9"

	"github.com/founderstack/api/internal/pkg/safego"
)

// HealthHandler answers GET /api/v1/health. The route needs no login, so a
// result is reused for healthCacheTTL — a flood of requests costs one round of
// probes, not one per request — and a failure is reported as a plain word:
// the underlying error (hostnames, users) goes to the log, not the caller.
type HealthHandler struct {
	db       *pgxpool.Pool
	redis    *redis.Client
	pinecone *pinecone.Client // nil when no PINECONE_API_KEY is configured

	mu     sync.Mutex
	cached *healthResult
	probe  func(context.Context) healthResult
}

type healthResult struct {
	status int
	body   gin.H
	at     time.Time
}

// NewHealthHandler builds a HealthHandler. pc may be nil — the Pinecone
// check then reports "skipped" instead of failing the overall status.
func NewHealthHandler(db *pgxpool.Pool, rdb *redis.Client, pc *pinecone.Client) *HealthHandler {
	h := &HealthHandler{db: db, redis: rdb, pinecone: pc}
	h.probe = h.runProbes
	return h
}

// Register mounts the health route on rg.
func (h *HealthHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/health", h.Check)
}

const (
	healthCheckTimeout = 5 * time.Second
	healthCacheTTL     = 5 * time.Second
)

// Check runs the database, Redis, and Pinecone probes concurrently and
// reports 200 when the two critical dependencies (database, Redis) are
// healthy, 503 otherwise. Pinecone is reported but never fails the overall
// status — RAG being briefly unreachable shouldn't take the whole API down.
func (h *HealthHandler) Check(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cached == nil || time.Since(h.cached.at) > healthCacheTTL {
		ctx, cancel := context.WithTimeout(c.Request.Context(), healthCheckTimeout)
		defer cancel()
		res := h.probe(ctx)
		res.at = time.Now()
		h.cached = &res
	}
	c.JSON(h.cached.status, h.cached.body)
}

func (h *HealthHandler) runProbes(ctx context.Context) healthResult {
	var wg sync.WaitGroup
	checks := map[string]string{
		"database": "unhealthy",
		"redis":    "unhealthy",
		"pinecone": "unhealthy",
	}
	var mu sync.Mutex
	set := func(key, value string) {
		mu.Lock()
		checks[key] = value
		mu.Unlock()
	}
	fail := func(key string, err error) {
		slog.Warn("health probe failed", "dependency", key, "error", err)
		set(key, "unhealthy")
	}

	wg.Add(3)
	safego.Go("health: database", func() {
		defer wg.Done()
		if err := h.db.Ping(ctx); err != nil {
			fail("database", err)
			return
		}
		set("database", "healthy")
	})
	safego.Go("health: redis", func() {
		defer wg.Done()
		if err := h.redis.Ping(ctx).Err(); err != nil {
			fail("redis", err)
			return
		}
		set("redis", "healthy")
	})
	safego.Go("health: pinecone", func() {
		defer wg.Done()
		if h.pinecone == nil {
			set("pinecone", "skipped (no API key in .env)")
			return
		}
		if _, err := h.pinecone.ListIndexes(ctx); err != nil {
			fail("pinecone", err)
			return
		}
		set("pinecone", "healthy")
	})
	wg.Wait()

	criticalHealthy := checks["database"] == "healthy" && checks["redis"] == "healthy"
	status := http.StatusServiceUnavailable
	statusText := "degraded"
	if criticalHealthy {
		status = http.StatusOK
		statusText = "healthy"
	}

	return healthResult{status: status, body: gin.H{"status": statusText, "checks": checks}}
}

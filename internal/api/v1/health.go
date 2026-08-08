package v1

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pinecone-io/go-pinecone/v5/pinecone"
	"github.com/redis/go-redis/v9"
)

// HealthHandler answers GET /api/v1/health by probing every external
// dependency the API needs at request time (no cached/stale status).
type HealthHandler struct {
	db       *pgxpool.Pool
	redis    *redis.Client
	pinecone *pinecone.Client // nil when no PINECONE_API_KEY is configured
}

// NewHealthHandler builds a HealthHandler. pc may be nil — the Pinecone
// check reports "skipped" rather than failing the overall health check when no API key is configured.
func NewHealthHandler(db *pgxpool.Pool, rdb *redis.Client, pc *pinecone.Client) *HealthHandler {
	return &HealthHandler{db: db, redis: rdb, pinecone: pc}
}

// Register mounts the health route on rg.
func (h *HealthHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/health", h.Check)
}

const healthCheckTimeout = 5 * time.Second

// Check runs the database, Redis, and Pinecone probes concurrently and
// reports 200 when the two critical dependencies (database, Redis) are
// healthy, 503 otherwise. Pinecone is reported but never fails the overall
// status — RAG being briefly unreachable shouldn't take the whole API down.
func (h *HealthHandler) Check(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), healthCheckTimeout)
	defer cancel()

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

	wg.Add(3)
	go func() {
		defer wg.Done()
		if err := h.db.Ping(ctx); err != nil {
			set("database", "unhealthy: "+err.Error())
			return
		}
		set("database", "healthy")
	}()
	go func() {
		defer wg.Done()
		if err := h.redis.Ping(ctx).Err(); err != nil {
			set("redis", "unhealthy: "+err.Error())
			return
		}
		set("redis", "healthy")
	}()
	go func() {
		defer wg.Done()
		if h.pinecone == nil {
			set("pinecone", "skipped (no API key in .env)")
			return
		}
		if _, err := h.pinecone.ListIndexes(ctx); err != nil {
			set("pinecone", "unhealthy: "+err.Error())
			return
		}
		set("pinecone", "healthy")
	}()
	wg.Wait()

	criticalHealthy := checks["database"] == "healthy" && checks["redis"] == "healthy"
	status := http.StatusServiceUnavailable
	statusText := "degraded"
	if criticalHealthy {
		status = http.StatusOK
		statusText = "healthy"
	}

	c.JSON(status, gin.H{"status": statusText, "checks": checks})
}

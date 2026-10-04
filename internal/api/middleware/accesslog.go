package middleware

import (
	"log/slog"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/founderstack/api/internal/pkg/errreport"
)

// AccessLog writes one structured line per request. It logs the route
// *template* (/api/v1/runs/:id), never the raw path or query string: those
// carry run ids, share-report tokens, OAuth codes and approval action tokens,
// none of which belong in logs. Health probes are skipped.
func AccessLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		if route == "/api/v1/health" {
			return
		}
		level := slog.LevelInfo
		if c.Writer.Status() >= 500 {
			level = slog.LevelError
			if _, done := c.Get(reportedKey); !done { // a recovered panic was already reported with its stack
				errreport.Message("http", "server error "+c.Request.Method+" "+route, map[string]string{"route": route, "status": strconv.Itoa(c.Writer.Status()), "request_id": c.Writer.Header().Get(requestIDHeader)})
			}
		}
		logger.Log(c.Request.Context(), level, "request",
			"method", c.Request.Method,
			"route", route,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"bytes", c.Writer.Size(),
			"request_id", c.Writer.Header().Get(requestIDHeader),
		)
	}
}

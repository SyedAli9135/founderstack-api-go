package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/config"
)

// Recovery catches panics so a single bad request can't take the process
// down, reporting them through the same envelope every other error uses.
func Recovery(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic recovered",
					"error", r,
					"request_id", response.RequestID(c),
					"stack", string(debug.Stack()),
				)
				message := "An unexpected internal server error occurred. Please contact support with the Request ID."
				if !cfg.IsProduction() {
					message = fmt.Sprintf("%v", r)
				}
				response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", message)
				c.Abort()
			}
		}()
		c.Next()
	}
}

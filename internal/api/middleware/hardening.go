package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/founderstack/api/internal/config"
)

// SecurityHeaders sets the response headers an API needs: it serves JSON
// only, so the CSP forbids everything and framing is denied outright.
// HSTS is production-only so local http:// development isn't pinned to https.
func SecurityHeaders(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		if cfg.IsProduction() {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		c.Next()
	}
}

// LimitBody caps every request body at maxBytes, so an unauthenticated
// caller can't make a handler buffer an unbounded payload (webhooks read
// the whole body before verifying the signature).
func LimitBody(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		}
		c.Next()
	}
}

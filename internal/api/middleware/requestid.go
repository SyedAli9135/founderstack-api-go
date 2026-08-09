// Package middleware holds cross-cutting Gin middleware shared by every route.
package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/founderstack/api/internal/api/response"
)

const requestIDHeader = "X-Request-ID"

// RequestID assigns every request a fresh opaque ID, stores it for handlers/error
// responses via response.RequestID, and echoes it back in the response
// header for client-side log correlation.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := response.NewID()
		response.SetRequestID(c, id)
		c.Writer.Header().Set(requestIDHeader, id)
		c.Next()
	}
}

package response

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/gin-gonic/gin"
)

const requestIDKey = "request_id"

// Success mirrors SuccessEnvelope: {"status": "success", "message": ..., "data": ...}.
type Success struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Data    any    `json:"data,omitempty"`
}

// ErrorDetail mirrors ErrorDetail: code, message, request ID, optional detail.
type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
	Detail    any    `json:"detail,omitempty"`
}

// Error mirrors ErrorEnvelope: {"status": "error", "error": {...}}.
type Error struct {
	Status string      `json:"status"`
	Error  ErrorDetail `json:"error"`
}

// OK writes a Success envelope. data may be nil.
func OK(c *gin.Context, httpStatus int, message string, data any) {
	c.JSON(httpStatus, Success{Status: "success", Message: message, Data: data})
}

// Fail writes an Error envelope, filling RequestID from the value set by
// the request-ID middleware.
func Fail(c *gin.Context, httpStatus int, code, message string) {
	c.JSON(httpStatus, Error{Status: "error", Error: ErrorDetail{
		Code:      code,
		Message:   message,
		RequestID: RequestID(c),
	}})
}

// FailWithDetail is Fail plus a structured detail payload (e.g. validation errors).
func FailWithDetail(c *gin.Context, httpStatus int, code, message string, detail any) {
	c.JSON(httpStatus, Error{Status: "error", Error: ErrorDetail{
		Code:      code,
		Message:   message,
		RequestID: RequestID(c),
		Detail:    detail,
	}})
}

// SetRequestID stores id on the request context. Called once, by the
// request-ID middleware.
func SetRequestID(c *gin.Context, id string) {
	c.Set(requestIDKey, id)
}

// RequestID returns the current request's ID, or "" if the middleware
// hasn't run (shouldn't happen outside tests).
func RequestID(c *gin.Context) string {
	id, _ := c.Get(requestIDKey)
	s, _ := id.(string)
	return s
}

// NewID returns a random 32-character hex ID (request IDs and similar).
func NewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

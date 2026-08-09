package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/founderstack/api/internal/api/response"
)

func TestRequestID_SetsHeaderAndContextValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())

	var seenInHandler string
	r.GET("/", func(c *gin.Context) {
		seenInHandler = response.RequestID(c)
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	header := rec.Header().Get("X-Request-ID")
	if header == "" {
		t.Fatal("X-Request-ID response header not set")
	}
	if seenInHandler == "" {
		t.Fatal("response.RequestID(c) was empty inside the handler")
	}
	if header != seenInHandler {
		t.Fatalf("header = %q, context value = %q, want them equal", header, seenInHandler)
	}
}

func TestRequestID_IgnoresClientSuppliedHeader(t *testing.T) {
	// A caller-supplied X-Request-ID must not be trusted — always generate
	// a fresh one, or a client could inject arbitrary values into server logs.
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "attacker-supplied-value")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-ID"); got == "attacker-supplied-value" {
		t.Fatal("middleware echoed back the client-supplied X-Request-ID instead of generating its own")
	}
}

func TestRequestID_DifferentPerRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	rec1 := httptest.NewRecorder()
	r.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/", nil))
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))

	id1 := rec1.Header().Get("X-Request-ID")
	id2 := rec2.Header().Get("X-Request-ID")
	if id1 == id2 {
		t.Fatalf("two separate requests got the same request ID: %q", id1)
	}
}

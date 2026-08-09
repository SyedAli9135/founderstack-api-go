package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/config"
)

func panicRouter(t *testing.T, cfg *config.Config) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	r.Use(Recovery(cfg))
	r.GET("/", func(c *gin.Context) {
		panic("something exploded")
	})
	return r
}

func TestRecovery_CatchesPanicAndReturns500(t *testing.T) {
	r := panicRouter(t, &config.Config{AppEnv: "development"})

	rec := httptest.NewRecorder()
	// A panicking handler must not crash the test process — if Recovery
	// doesn't catch it, this call itself panics and fails the test.
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var got response.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got.Error.Code != "INTERNAL_SERVER_ERROR" {
		t.Fatalf("code = %q, want INTERNAL_SERVER_ERROR", got.Error.Code)
	}
	if got.Error.RequestID == "" {
		t.Fatal("request_id was empty — Recovery should run after RequestID middleware")
	}
}

func TestRecovery_ExposesPanicMessageInDevelopment(t *testing.T) {
	r := panicRouter(t, &config.Config{AppEnv: "development"})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	var got response.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got.Error.Message != "something exploded" {
		t.Fatalf("message = %q, want the raw panic value in development", got.Error.Message)
	}
}

func TestRecovery_MasksPanicMessageInProduction(t *testing.T) {
	r := panicRouter(t, &config.Config{AppEnv: "production"})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	var got response.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got.Error.Message == "something exploded" {
		t.Fatal("production response leaked the raw panic message — internal detail should be masked")
	}
	if got.Error.Message == "" {
		t.Fatal("production response should still have a generic message, not an empty one")
	}
}

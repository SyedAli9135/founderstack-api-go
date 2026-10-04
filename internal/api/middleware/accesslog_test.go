package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAccessLog_LogsTheRouteTemplateNeverTheSecretsInThePath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var buf bytes.Buffer
	r := gin.New()
	r.Use(RequestID(), AccessLog(slog.New(slog.NewTextHandler(&buf, nil))))
	r.GET("/api/public/reports/:token", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/api/v1/health", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/public/reports/SECRET-SHARE-TOKEN?action_token=SECRET-ACTION&code=SECRET-CODE", nil))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	out := buf.String()
	if !strings.Contains(out, "route=/api/public/reports/:token") || !strings.Contains(out, "status=200") {
		t.Fatalf("access line missing the route template or status: %q", out)
	}
	for _, secret := range []string{"SECRET-SHARE-TOKEN", "SECRET-ACTION", "SECRET-CODE"} {
		if strings.Contains(out, secret) {
			t.Fatalf("access log leaked %q: %q", secret, out)
		}
	}
	if strings.Count(out, "request") != 1 && strings.Contains(out, "health") {
		t.Fatalf("health probe should not be logged: %q", out)
	}
}

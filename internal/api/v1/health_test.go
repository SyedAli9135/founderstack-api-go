package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestHealth_ReusesAResultInsteadOfProbingPerRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var probes atomic.Int32
	h := &HealthHandler{probe: func(context.Context) healthResult {
		probes.Add(1)
		return healthResult{status: http.StatusOK, body: gin.H{"status": "healthy"}}
	}}
	r := gin.New()
	h.Register(r.Group("/api/v1"))

	for i := 0; i < 50; i++ {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i, rec.Code)
		}
	}
	if n := probes.Load(); n != 1 {
		t.Fatalf("probes = %d for 50 requests, want 1", n)
	}
}

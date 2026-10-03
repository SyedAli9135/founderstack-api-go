package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/founderstack/api/internal/config"
)

func TestSecurityHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		env      string
		wantHSTS bool
	}{{"production", true}, {"development", false}} {
		r := gin.New()
		r.Use(SecurityHeaders(&config.Config{AppEnv: tc.env}))
		r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", tc.env, got)
		}
		if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q", tc.env, got)
		}
		if got := w.Header().Get("Strict-Transport-Security") != ""; got != tc.wantHSTS {
			t.Errorf("%s: HSTS present = %v, want %v", tc.env, got, tc.wantHSTS)
		}
	}
}

func TestLimitBody_RejectsOversizedRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(LimitBody(10))
	r.POST("/", func(c *gin.Context) {
		if _, err := c.GetRawData(); err != nil {
			c.Status(http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusOK)
	})

	for body, want := range map[string]int{
		"short":                  http.StatusOK,
		strings.Repeat("x", 100): http.StatusRequestEntityTooLarge,
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
		if w.Code != want {
			t.Errorf("body len %d: status = %d, want %d", len(body), w.Code, want)
		}
	}
}

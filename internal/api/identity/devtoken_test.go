package identity

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/pkg/devtoken"
)

func testRouter(cfg *config.Config) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewDevTokenHandler(cfg).Register(r.Group("/api/v1/auth"))
	return r
}

func postDevToken(t *testing.T, router *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/dev-token", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestDevTokenHandler_DisabledInProduction(t *testing.T) {
	cfg := &config.Config{AppEnv: "production", DevTokenSecret: "some-secret"}
	rec := postDevToken(t, testRouter(cfg), `{"clerk_user_id":"user_abc"}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (must not reveal this route exists in production)", rec.Code)
	}
}

func TestDevTokenHandler_DisabledWhenSecretUnset(t *testing.T) {
	cfg := &config.Config{AppEnv: "development", DevTokenSecret: ""}
	rec := postDevToken(t, testRouter(cfg), `{"clerk_user_id":"user_abc"}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDevTokenHandler_RejectsMissingClerkUserID(t *testing.T) {
	cfg := &config.Config{AppEnv: "development", DevTokenSecret: "dev-secret"}
	rec := postDevToken(t, testRouter(cfg), `{}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestDevTokenHandler_MintsAVerifiableToken(t *testing.T) {
	cfg := &config.Config{AppEnv: "development", DevTokenSecret: "dev-secret"}
	rec := postDevToken(t, testRouter(cfg), `{"clerk_user_id":"user_abc123"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got.Data.Token == "" {
		t.Fatal("response contained no token")
	}

	sub, err := devtoken.Verify(cfg.DevTokenSecret.Expose(), got.Data.Token)
	if err != nil {
		t.Fatalf("the minted token failed devtoken.Verify: %v", err)
	}
	if sub != "user_abc123" {
		t.Fatalf("verified subject = %q, want %q", sub, "user_abc123")
	}
}

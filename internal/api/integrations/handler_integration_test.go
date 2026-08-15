package integrations

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/core/integrations"
	"github.com/founderstack/api/internal/pkg/devtoken"
)

type fakeOAuthProvider struct {
	name          string
	exchangeToken *integrations.Token
	exchangeErr   error
	revokedTokens []string
	revokeErr     error
	validateErr   error
}

func (f *fakeOAuthProvider) Name() string { return f.name }
func (f *fakeOAuthProvider) GetAuthURL(state string) string {
	return "https://example.com/authorize?state=" + state
}
func (f *fakeOAuthProvider) ExchangeCode(ctx context.Context, code string) (*integrations.Token, error) {
	if f.exchangeErr != nil {
		return nil, f.exchangeErr
	}
	return f.exchangeToken, nil
}
func (f *fakeOAuthProvider) RevokeToken(ctx context.Context, token string) error {
	f.revokedTokens = append(f.revokedTokens, token)
	return f.revokeErr
}
func (f *fakeOAuthProvider) ValidateToken(ctx context.Context, token string) error {
	return f.validateErr
}

type fakeKeyProvider struct {
	name        string
	validateErr error
}

func (f *fakeKeyProvider) Name() string                                      { return f.name }
func (f *fakeKeyProvider) ValidateKey(ctx context.Context, key string) error { return f.validateErr }

func testSystemPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_SYSTEM_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_SYSTEM_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to system test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testAppPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_APP_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_APP_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to app test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis not reachable at localhost:6379 (run make docker-up): %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func testEncryptionKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func testOrgAndUser(t *testing.T, systemPool *pgxpool.Pool) (orgID pgtype.UUID, clerkUserID string) {
	t.Helper()
	suffix := response.NewID()[:12]
	orgClerkID := "org_workflow4_test_" + suffix
	clerkUserID = "user_workflow4_test_" + suffix
	ctx := context.Background()

	err := systemPool.QueryRow(ctx,
		"insert into organizations (clerk_org_id, name, slug) values ($1, 'Workflow4 Test Org', $2) returning id",
		orgClerkID, "workflow4-test-"+suffix,
	).Scan(&orgID)
	if err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	_, err = systemPool.Exec(ctx,
		`insert into users (org_id, clerk_user_id, email) values ($1, $2, 'workflow4-test@example.com')`,
		orgID, clerkUserID,
	)
	if err != nil {
		t.Fatalf("insert test user: %v", err)
	}

	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from users where clerk_user_id = $1", clerkUserID)
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where clerk_org_id = $1", orgClerkID)
	})
	return orgID, clerkUserID
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{AppEnv: "development", DevTokenSecret: "test-dev-token-secret"}
}

func testRouter(t *testing.T, systemPool, appPool *pgxpool.Pool, rdb *redis.Client, cfg *config.Config, encKey []byte, registry *integrations.Registry, frontendURL string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())

	stateManager := integrations.NewStateManager(rdb, "test-oauth-state-secret")
	h := NewHandler(appPool, encKey, registry, stateManager, frontendURL)

	authed := r.Group("/api/v1/integrations")
	authed.Use(middleware.RequireAuth(systemPool, cfg))
	h.Register(authed)

	callback := r.Group("/api/v1/integrations")
	h.RegisterCallback(callback)

	return r
}

func authedRequest(t *testing.T, cfg *config.Config, clerkUserID, method, path string, body any) *http.Request {
	t.Helper()
	token, err := devtoken.Sign(cfg.DevTokenSecret.Expose(), clerkUserID)
	if err != nil {
		t.Fatalf("sign dev token: %v", err)
	}

	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestIntegrationsHandler_FullLifecycle(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	rdb := testRedis(t)
	cfg := testConfig(t)
	encKey := testEncryptionKey(t)
	_, clerkUserID := testOrgAndUser(t, systemPool)

	fakeSlack := &fakeOAuthProvider{
		name:          "slack",
		exchangeToken: &integrations.Token{AccessToken: "xoxb-fake-token"},
	}
	fakeStripe := &fakeKeyProvider{name: "stripe"}
	registry := integrations.NewRegistry(fakeSlack, fakeStripe)

	router := testRouter(t, systemPool, appPool, rdb, cfg, encKey, registry, "http://localhost:3000")

	t.Run("unauthenticated list is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("list before any connection shows not_connected for the whole catalog", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/integrations", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Data []integrationView `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Data) != len(integrations.Catalog) {
			t.Fatalf("got %d entries, want %d (full catalog)", len(got.Data), len(integrations.Catalog))
		}
		for _, v := range got.Data {
			if v.Status != "not_connected" {
				t.Fatalf("service %s status = %s, want not_connected", v.Service, v.Status)
			}
		}
	})

	var stateFromConnect string
	t.Run("connect returns a redirect_url embedding state", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/integrations/slack/connect", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Data struct {
				RedirectURL string `json:"redirect_url"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		parsed, err := url.Parse(got.Data.RedirectURL)
		if err != nil {
			t.Fatalf("parse redirect_url: %v", err)
		}
		stateFromConnect = parsed.Query().Get("state")
		if stateFromConnect == "" {
			t.Fatal("redirect_url has no state param")
		}
	})

	t.Run("connect on a non-OAuth service is rejected", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/integrations/stripe/connect", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("callback with mismatched service is rejected", func(t *testing.T) {
		// Mints its own state (via a separate /connect call) rather than
		// reusing stateFromConnect — state is one-time-use, consumed by
		// Verify regardless of what the mismatch check below decides, so
		// reusing it here would burn the token the later "successful
		// callback" subtest still needs.
		connectReq := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/integrations/slack/connect", nil)
		connectRec := httptest.NewRecorder()
		router.ServeHTTP(connectRec, connectReq)
		var connectGot struct {
			Data struct {
				RedirectURL string `json:"redirect_url"`
			} `json:"data"`
		}
		if err := json.Unmarshal(connectRec.Body.Bytes(), &connectGot); err != nil {
			t.Fatal(err)
		}
		parsed, err := url.Parse(connectGot.Data.RedirectURL)
		if err != nil {
			t.Fatalf("parse redirect_url: %v", err)
		}
		mismatchState := parsed.Query().Get("state")

		// Presented against the "notion" callback path — a state minted
		// for "slack" must be rejected, not silently connect notion.
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/integrations/notion/callback?code=fake&state="+url.QueryEscape(mismatchState), nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("callback completes the connection and redirects", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/integrations/slack/callback?code=fake-code&state="+url.QueryEscape(stateFromConnect), nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302; body = %s", rec.Code, rec.Body.String())
		}
		loc := rec.Header().Get("Location")
		if loc != "http://localhost:3000/integrations?connected=slack" {
			t.Fatalf("Location = %q, want the connected=slack redirect", loc)
		}
	})

	t.Run("replaying the same state fails (one-time use)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/integrations/slack/callback?code=fake-code&state="+url.QueryEscape(stateFromConnect), nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("list now shows slack connected", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/integrations", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var got struct {
			Data []integrationView `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, v := range got.Data {
			if v.Service == "slack" {
				found = true
				if v.Status != "connected" {
					t.Fatalf("slack status = %s, want connected", v.Status)
				}
			}
		}
		if !found {
			t.Fatal("slack missing from list")
		}
	})

	t.Run("status validates against the live provider", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/integrations/slack/status", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var got struct {
			Data statusResponse `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Data.Status != "connected" {
			t.Fatalf("status = %s, want connected; body = %s", got.Data.Status, rec.Body.String())
		}
	})

	t.Run("stripe api-key connects via KeyProvider path", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/integrations/stripe/api-key",
			map[string]string{"key": "sk_test_fake"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("api-key on an OAuth service is rejected", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/integrations/slack/api-key",
			map[string]string{"key": "whatever"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("api-key with a rejected key does not save a connection", func(t *testing.T) {
		fakeStripe.validateErr = errors.New("fake: key rejected")
		defer func() { fakeStripe.validateErr = nil }()

		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/integrations/stripe/api-key",
			map[string]string{"key": "sk_test_bad"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("disconnect revokes then deactivates", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodDelete, "/api/v1/integrations/slack", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}
		if len(fakeSlack.revokedTokens) != 1 || fakeSlack.revokedTokens[0] != "xoxb-fake-token" {
			t.Fatalf("revokedTokens = %v, want exactly [xoxb-fake-token]", fakeSlack.revokedTokens)
		}
	})

	t.Run("status after disconnect reports revoked without re-validating", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/integrations/slack/status", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var got struct {
			Data statusResponse `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Data.Status != "revoked" {
			t.Fatalf("status = %s, want revoked", got.Data.Status)
		}
	})
}

func TestIntegrationsHandler_CrossOrgIsolation(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	rdb := testRedis(t)
	cfg := testConfig(t)
	encKey := testEncryptionKey(t)

	_, userA := testOrgAndUser(t, systemPool)
	_, userB := testOrgAndUser(t, systemPool)

	fakeStripe := &fakeKeyProvider{name: "stripe"}
	registry := integrations.NewRegistry(fakeStripe)
	router := testRouter(t, systemPool, appPool, rdb, cfg, encKey, registry, "http://localhost:3000")

	reqA := authedRequest(t, cfg, userA, http.MethodPost, "/api/v1/integrations/stripe/api-key",
		map[string]string{"key": "sk_test_org_a_only"})
	recA := httptest.NewRecorder()
	router.ServeHTTP(recA, reqA)
	if recA.Code != http.StatusCreated {
		t.Fatalf("org A connect: status = %d, want 201; body = %s", recA.Code, recA.Body.String())
	}

	reqB := authedRequest(t, cfg, userB, http.MethodGet, "/api/v1/integrations/stripe/status", nil)
	recB := httptest.NewRecorder()
	router.ServeHTTP(recB, reqB)
	var got struct {
		Data statusResponse `json:"data"`
	}
	if err := json.Unmarshal(recB.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Data.Status != "not_connected" {
		t.Fatalf("org B saw status = %s, want not_connected — cross-tenant leak", got.Data.Status)
	}
}

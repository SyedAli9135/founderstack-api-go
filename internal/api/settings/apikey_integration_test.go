package settings

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/pkg/devtoken"
)

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

func testEncryptionKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		AppEnv:           "development",
		DevTokenSecret:   "test-dev-token-secret",
		APIKeyMockPrefix: "mock-test-key-",
	}
}

// testOrgAndUser inserts a fresh org+user directly via systemPool
// (BYPASSRLS, same as how the real Clerk webhook creates them) and
// registers cleanup. Returns the user's clerk_user_id, ready to mint a dev
// token for.
func testOrgAndUser(t *testing.T, systemPool *pgxpool.Pool) (clerkUserID string) {
	t.Helper()
	suffix := response.NewID()[:12]
	orgClerkID := "org_settings_test_" + suffix
	clerkUserID = "user_settings_test_" + suffix
	ctx := context.Background()

	_, err := systemPool.Exec(ctx,
		"insert into organizations (clerk_org_id, name, slug) values ($1, 'Settings Test Org', $2)",
		orgClerkID, "settings-test-"+suffix,
	)
	if err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	_, err = systemPool.Exec(ctx,
		`insert into users (org_id, clerk_user_id, email)
		 select id, $2, 'settings-test@example.com' from organizations where clerk_org_id = $1`,
		orgClerkID, clerkUserID,
	)
	if err != nil {
		t.Fatalf("insert test user: %v", err)
	}

	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from users where clerk_user_id = $1", clerkUserID)
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where clerk_org_id = $1", orgClerkID)
	})
	return clerkUserID
}

func testRouter(t *testing.T, systemPool, appPool *pgxpool.Pool, cfg *config.Config, encryptionKey []byte) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())
	rg := r.Group("/api/v1/settings")
	rg.Use(middleware.RequireAuth(systemPool, cfg))
	NewHandler(appPool, encryptionKey, cfg.APIKeyMockPrefix).Register(rg)
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

func TestSettingsAPIKey_FullLifecycle(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	cfg := testConfig(t)
	encKey := testEncryptionKey(t)
	router := testRouter(t, systemPool, appPool, cfg, encKey)
	clerkUserID := testOrgAndUser(t, systemPool)

	t.Run("unauthenticated request is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/api-key/status", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("status before any key exists is null, not an error", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/settings/api-key/status", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Data any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Data != nil {
			t.Fatalf("data = %v, want nil", got.Data)
		}
	})

	t.Run("rejects an obviously malformed key without a DB write", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/settings/api-key",
			map[string]string{"api_key": "hunter2"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("submitting a mock-prefixed key succeeds and encrypts at rest", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/settings/api-key",
			map[string]string{"api_key": "mock-test-key-abcdefghijklmnop"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
		}
		if bytes.Contains(rec.Body.Bytes(), []byte("abcdefghijklmnop")) {
			t.Fatal("response body contains the raw key — must return only the prefix")
		}

		// Verified via systemPool (BYPASSRLS), not appPool: appPool is
		// RLS-enforced and this is a plain query with no tenant.WithTx
		// wrapping it, so querying it directly here would correctly see
		// zero rows regardless of whether the write succeeded — that's
		// RLS's deny-by-default working as intended, not a check on it.
		// The 201 above is what proves the write happened through the
		// real (tenant-scoped) path; this only confirms what got stored.
		var encrypted string
		err := systemPool.QueryRow(context.Background(),
			`select k.encrypted_key from api_key_registry k
			 join organizations o on o.id = k.org_id
			 join users u on u.org_id = o.id
			 where u.clerk_user_id = $1`, clerkUserID,
		).Scan(&encrypted)
		if err != nil {
			t.Fatalf("query encrypted_key: %v", err)
		}
		if bytes.Contains([]byte(encrypted), []byte("abcdefghijklmnop")) {
			t.Fatal("plaintext key found in the stored encrypted_key column")
		}
	})

	t.Run("status after submission reflects the key", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/settings/api-key/status", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Data struct {
				Provider  string `json:"provider"`
				IsValid   bool   `json:"is_valid"`
				KeyPrefix string `json:"key_prefix"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Data.Provider != "anthropic" || !got.Data.IsValid || got.Data.KeyPrefix != "mock-tes..." {
			t.Fatalf("status data = %+v, want provider=anthropic is_valid=true key_prefix=mock-tes...", got.Data)
		}
	})

	t.Run("delete deactivates the key", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodDelete, "/api/v1/settings/api-key", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("204 response has a body: %s", rec.Body.String())
		}
	})

	t.Run("status after delete shows is_valid=false, not null", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/settings/api-key/status", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var got struct {
			Data struct {
				IsValid bool `json:"is_valid"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Data.IsValid {
			t.Fatal("is_valid = true after delete, want false")
		}
	})
}

func TestSettingsAPIKey_CrossOrgIsolation(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	cfg := testConfig(t)
	encKey := testEncryptionKey(t)
	router := testRouter(t, systemPool, appPool, cfg, encKey)

	userA := testOrgAndUser(t, systemPool)
	userB := testOrgAndUser(t, systemPool)

	// Org A submits a key.
	reqA := authedRequest(t, cfg, userA, http.MethodPost, "/api/v1/settings/api-key",
		map[string]string{"api_key": "mock-test-key-org-a-only-key"})
	recA := httptest.NewRecorder()
	router.ServeHTTP(recA, reqA)
	if recA.Code != http.StatusCreated {
		t.Fatalf("org A submit: status = %d, want 201; body = %s", recA.Code, recA.Body.String())
	}

	// Org B must see nothing — not org A's key, not an error.
	reqB := authedRequest(t, cfg, userB, http.MethodGet, "/api/v1/settings/api-key/status", nil)
	recB := httptest.NewRecorder()
	router.ServeHTTP(recB, reqB)
	if recB.Code != http.StatusOK {
		t.Fatalf("org B status: status = %d, want 200; body = %s", recB.Code, recB.Body.String())
	}
	var got struct {
		Data any `json:"data"`
	}
	if err := json.Unmarshal(recB.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Data != nil {
		t.Fatalf("org B saw data = %v, want nil — cross-tenant leak", got.Data)
	}
}

func TestSettingsAPIKey_UnsyncedUserRejected(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	cfg := testConfig(t)
	encKey := testEncryptionKey(t)
	router := testRouter(t, systemPool, appPool, cfg, encKey)

	// A dev token for a clerk_user_id that was never synced via webhook —
	// must fail at RequireAuth's USER_NOT_SYNCHRONIZED step.
	req := authedRequest(t, cfg, "user_never_synced_"+response.NewID()[:12], http.MethodGet, "/api/v1/settings/api-key/status", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("USER_NOT_SYNCHRONIZED")) {
		t.Fatalf("body = %s, want it to contain USER_NOT_SYNCHRONIZED", rec.Body.String())
	}
}

// TestSettingsAPIKey_MultiProvider covers the 2026-08-21 generalization
// from Anthropic-only to 5 providers: multiple providers' keys coexisting
// per org, status lookups scoped by provider (not "whichever is active"),
// and the real bug this generalization could easily have introduced —
// deleting a non-active provider's key must not clobber a different,
// still-active provider's pointer on organizations.
func TestSettingsAPIKey_MultiProvider(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	cfg := testConfig(t)
	encKey := testEncryptionKey(t)
	router := testRouter(t, systemPool, appPool, cfg, encKey)
	clerkUserID := testOrgAndUser(t, systemPool)

	activeProvider := func(t *testing.T) *string {
		t.Helper()
		var p *string
		err := systemPool.QueryRow(context.Background(),
			`select o.llm_provider from organizations o
			 join users u on u.org_id = o.id
			 where u.clerk_user_id = $1`, clerkUserID,
		).Scan(&p)
		if err != nil {
			t.Fatalf("query llm_provider: %v", err)
		}
		return p
	}

	t.Run("rejects an unknown provider", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/settings/api-key",
			map[string]string{"provider": "made-up-llm", "api_key": "mock-test-key-whatever"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("submits an Anthropic key with no provider field (backward compat)", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/settings/api-key",
			map[string]string{"api_key": "mock-test-key-anthropic-key"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
		}
		if p := activeProvider(t); p == nil || *p != "anthropic" {
			t.Fatalf("active provider = %v, want anthropic", p)
		}
	})

	t.Run("submits an OpenAI key alongside it", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/settings/api-key",
			map[string]string{"provider": "openai", "api_key": "mock-test-key-openai-key"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
		}
		if p := activeProvider(t); p == nil || *p != "openai" {
			t.Fatalf("active provider = %v, want openai (most recently submitted)", p)
		}
	})

	t.Run("both providers' keys coexist independently", func(t *testing.T) {
		for _, provider := range []string{"anthropic", "openai"} {
			req := authedRequest(t, cfg, clerkUserID, http.MethodGet,
				"/api/v1/settings/api-key/status?provider="+provider, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			var got struct {
				Data struct {
					Provider string `json:"provider"`
					IsValid  bool   `json:"is_valid"`
				} `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Data.Provider != provider || !got.Data.IsValid {
				t.Fatalf("provider %s status = %+v, want provider=%s is_valid=true", provider, got.Data, provider)
			}
		}
	})

	t.Run("an unrecognized provider on status is rejected, not treated as no-key", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet,
			"/api/v1/settings/api-key/status?provider=made-up-llm", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("deleting the non-active provider's key does not clobber the active one", func(t *testing.T) {
		// OpenAI is currently active (submitted last, above). Deleting
		// Anthropic's key must leave organizations.llm_provider untouched.
		req := authedRequest(t, cfg, clerkUserID, http.MethodDelete, "/api/v1/settings/api-key?provider=anthropic", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}
		if p := activeProvider(t); p == nil || *p != "openai" {
			t.Fatalf("active provider = %v, want openai (deleting anthropic must not clear it)", p)
		}
	})

	t.Run("deleting the active provider's key does clear the org's active pointer", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodDelete, "/api/v1/settings/api-key?provider=openai", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}
		if p := activeProvider(t); p != nil {
			t.Fatalf("active provider = %v, want nil after deleting the active provider's key", *p)
		}
	})
}

type providerListEntry struct {
	Provider      string  `json:"provider"`
	Name          string  `json:"name"`
	KeyPrefixHint string  `json:"key_prefix_hint"`
	IsConfigured  bool    `json:"is_configured"`
	IsValid       bool    `json:"is_valid"`
	IsActive      bool    `json:"is_active"`
	KeyPrefix     *string `json:"key_prefix"`
}

func TestSettingsAPIKey_ListProviders(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	cfg := testConfig(t)
	encKey := testEncryptionKey(t)
	router := testRouter(t, systemPool, appPool, cfg, encKey)
	clerkUserID := testOrgAndUser(t, systemPool)

	fetchProviders := func(t *testing.T) []providerListEntry {
		t.Helper()
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/settings/api-key/providers", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Data []providerListEntry `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got.Data
	}

	t.Run("lists all 5 catalog providers before any key is configured", func(t *testing.T) {
		providers := fetchProviders(t)
		if len(providers) != 5 {
			t.Fatalf("len(providers) = %d, want 5; got %+v", len(providers), providers)
		}
		want := map[string]bool{"anthropic": false, "openai": false, "gemini": false, "qwen": false, "deepseek": false}
		for _, p := range providers {
			if _, ok := want[p.Provider]; !ok {
				t.Errorf("unexpected provider %q in list", p.Provider)
			}
			want[p.Provider] = true
			if p.IsConfigured || p.IsValid || p.IsActive || p.KeyPrefix != nil {
				t.Errorf("provider %q = %+v, want all-false/nil before any submission", p.Provider, p)
			}
			if p.Name == "" || p.KeyPrefixHint == "" {
				t.Errorf("provider %q missing catalog metadata: %+v", p.Provider, p)
			}
		}
		for provider, seen := range want {
			if !seen {
				t.Errorf("provider %q missing from list", provider)
			}
		}
	})

	t.Run("reflects submitted keys and which one is active", func(t *testing.T) {
		for _, body := range []map[string]string{
			{"api_key": "mock-test-key-anthropic"}, // no provider field -> defaults anthropic
			{"provider": "openai", "api_key": "mock-test-key-openai-here"},
		} {
			req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/settings/api-key", body)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("submit %v: status = %d, want 201; body = %s", body, rec.Code, rec.Body.String())
			}
		}

		byProvider := make(map[string]providerListEntry)
		for _, p := range fetchProviders(t) {
			byProvider[p.Provider] = p
		}

		anthropic := byProvider["anthropic"]
		if !anthropic.IsConfigured || !anthropic.IsValid || anthropic.IsActive || anthropic.KeyPrefix == nil {
			t.Errorf("anthropic = %+v, want configured+valid, not active (openai submitted after it)", anthropic)
		}
		openai := byProvider["openai"]
		if !openai.IsConfigured || !openai.IsValid || !openai.IsActive || openai.KeyPrefix == nil {
			t.Errorf("openai = %+v, want configured+valid+active (submitted most recently)", openai)
		}
		gemini := byProvider["gemini"]
		if gemini.IsConfigured || gemini.KeyPrefix != nil {
			t.Errorf("gemini = %+v, want untouched", gemini)
		}
	})
}

func TestSettingsAPIKey_ListProviders_CrossOrgIsolation(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	cfg := testConfig(t)
	encKey := testEncryptionKey(t)
	router := testRouter(t, systemPool, appPool, cfg, encKey)

	userA := testOrgAndUser(t, systemPool)
	userB := testOrgAndUser(t, systemPool)

	reqA := authedRequest(t, cfg, userA, http.MethodPost, "/api/v1/settings/api-key",
		map[string]string{"api_key": "mock-test-key-org-a-isolation"})
	recA := httptest.NewRecorder()
	router.ServeHTTP(recA, reqA)
	if recA.Code != http.StatusCreated {
		t.Fatalf("org A submit: status = %d, want 201; body = %s", recA.Code, recA.Body.String())
	}

	reqB := authedRequest(t, cfg, userB, http.MethodGet, "/api/v1/settings/api-key/providers", nil)
	recB := httptest.NewRecorder()
	router.ServeHTTP(recB, reqB)
	var got struct {
		Data []providerListEntry `json:"data"`
	}
	if err := json.Unmarshal(recB.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, p := range got.Data {
		if p.IsConfigured {
			t.Fatalf("org B sees provider %q as configured — cross-tenant leak: %+v", p.Provider, got.Data)
		}
	}
}

//go:build integration

package templates

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/pkg/devtoken"
)

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

func randSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// testOrgAndUser inserts a fresh org + one user of the given role.
// agent_templates itself is global (see handler.go's own package doc —
// no org_id, seeded once by migration 000015), so this fixture only ever
// needs a real org for the *installed agent* half of these tests, not
// for the templates themselves.
func testOrgAndUser(t *testing.T, systemPool *pgxpool.Pool, role string) (orgID pgtype.UUID, clerkUserID string) {
	t.Helper()
	suffix := randSuffix(t)
	clerkUserID = "user_templates_test_" + suffix
	ctx := context.Background()

	if err := systemPool.QueryRow(ctx,
		"insert into organizations (clerk_org_id, name, slug) values ($1, 'Templates Test Org', $2) returning id",
		"org_templates_test_"+suffix, "templates-test-"+suffix,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	if _, err := systemPool.Exec(ctx,
		`insert into users (org_id, clerk_user_id, email, role) values ($1, $2, 'templates-test@example.com', $3)`,
		orgID, clerkUserID, role,
	); err != nil {
		t.Fatalf("insert test user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID)
	})
	return orgID, clerkUserID
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{AppEnv: "development", DevTokenSecret: "test-dev-token-secret"}
}

func testRouter(t *testing.T, systemPool, appPool *pgxpool.Pool, cfg *config.Config) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())

	authed := r.Group("/api/v1")
	authed.Use(middleware.RequireAuth(systemPool, cfg))
	NewHandler(appPool).Register(authed)

	return r
}

func authedRequest(t *testing.T, cfg *config.Config, clerkUserID, method, path string) *http.Request {
	t.Helper()
	token, err := devtoken.Sign(cfg.DevTokenSecret.Expose(), clerkUserID)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

type apiEnvelope struct {
	Data json.RawMessage `json:"data"`
}
type apiError struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

func mustUnmarshalData(t *testing.T, body []byte, v any) {
	t.Helper()
	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v (body=%s)", err, body)
	}
	if err := json.Unmarshal(env.Data, v); err != nil {
		t.Fatalf("unmarshal data: %v (body=%s)", err, body)
	}
}

func TestTemplatesHandler_ListReturnsSeededCatalog(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	_, clerkUserID := testOrgAndUser(t, systemPool, "member")
	r := testRouter(t, systemPool, appPool, cfg)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/templates"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var list []templateSummary
	mustUnmarshalData(t, w.Body.Bytes(), &list)
	// Migration 000015 seeds 8 (one per real MCP tool server), migration
	// 000016 adds 2 more (Stripe/GitHub write-side tools no template
	// exercised yet) for full 18/18 real-tool coverage — 10 total. This
	// is global data every org (including a brand-new one, like this
	// test's own fixture) sees identically.
	if len(list) != 10 {
		t.Fatalf("List returned %d templates, want 10", len(list))
	}
	found := false
	for _, tmpl := range list {
		if tmpl.Name == "GitHub PR Reviewer" {
			found = true
			if !tmpl.IsFeatured {
				t.Fatal("GitHub PR Reviewer should be featured")
			}
			if tmpl.ToolCount != 2 {
				t.Fatalf("GitHub PR Reviewer tool_count = %d, want 2", tmpl.ToolCount)
			}
		}
	}
	if !found {
		t.Fatal("List did not include GitHub PR Reviewer")
	}
}

func TestTemplatesHandler_ListFiltersByCategory(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	_, clerkUserID := testOrgAndUser(t, systemPool, "member")
	r := testRouter(t, systemPool, appPool, cfg)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/templates?category=Ops"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var list []templateSummary
	mustUnmarshalData(t, w.Body.Bytes(), &list)
	if len(list) != 3 {
		t.Fatalf("List(category=Ops) returned %d, want 3 (Notion/Calendar/Drive)", len(list))
	}
	for _, tmpl := range list {
		if tmpl.Category != "Ops" {
			t.Fatalf("List(category=Ops) included %q, category=%q", tmpl.Name, tmpl.Category)
		}
	}
}

func TestTemplatesHandler_GetReturnsFullDetail(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	_, clerkUserID := testOrgAndUser(t, systemPool, "member")
	r := testRouter(t, systemPool, appPool, cfg)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/templates"))
	var list []templateSummary
	mustUnmarshalData(t, w.Body.Bytes(), &list)
	var stripeID string
	for _, tmpl := range list {
		if tmpl.Name == "Weekly Revenue Summary" {
			stripeID = tmpl.ID
		}
	}
	if stripeID == "" {
		t.Fatal("Weekly Revenue Summary not found in List")
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/templates/"+stripeID))
	if w.Code != http.StatusOK {
		t.Fatalf("Get status = %d, body = %s", w.Code, w.Body.String())
	}
	var detail templateDetail
	mustUnmarshalData(t, w.Body.Bytes(), &detail)
	if detail.SystemPrompt == "" {
		t.Fatal("Get did not return a system_prompt")
	}
	want := map[string]bool{"stripe.get_mrr": true, "stripe.list_subscriptions": true}
	if len(detail.AllowedTools) != 2 {
		t.Fatalf("Get allowed_tools = %v, want exactly stripe.get_mrr and stripe.list_subscriptions", detail.AllowedTools)
	}
	for _, tool := range detail.AllowedTools {
		if !want[tool] {
			t.Fatalf("Get allowed_tools included unexpected %q", tool)
		}
	}
}

func TestTemplatesHandler_GetUnknownIDReturns404(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	_, clerkUserID := testOrgAndUser(t, systemPool, "member")
	r := testRouter(t, systemPool, appPool, cfg)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/templates/00000000-0000-0000-0000-000000000000"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestTemplatesHandler_InstallCreatesAgentFromTemplate(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	_, clerkUserID := testOrgAndUser(t, systemPool, "admin")
	r := testRouter(t, systemPool, appPool, cfg)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/templates"))
	var list []templateSummary
	mustUnmarshalData(t, w.Body.Bytes(), &list)
	var slackID string
	for _, tmpl := range list {
		if tmpl.Name == "Slack Daily Brief" {
			slackID = tmpl.ID
		}
	}
	if slackID == "" {
		t.Fatal("Slack Daily Brief not found")
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/templates/"+slackID+"/install"))
	if w.Code != http.StatusCreated {
		t.Fatalf("Install status = %d, body = %s", w.Code, w.Body.String())
	}
	var installed installResponse
	mustUnmarshalData(t, w.Body.Bytes(), &installed)
	if installed.AgentID == "" {
		t.Fatal("Install returned no agent_id")
	}

	var name, systemPrompt string
	var policyScope []byte
	if err := systemPool.QueryRow(context.Background(),
		"select name, system_prompt, policy_scope from agents where id = $1", installed.AgentID,
	).Scan(&name, &systemPrompt, &policyScope); err != nil {
		t.Fatalf("query installed agent: %v", err)
	}
	if name != "Slack Daily Brief" {
		t.Fatalf("installed agent name = %q, want %q", name, "Slack Daily Brief")
	}
	if systemPrompt == "" {
		t.Fatal("installed agent has no system_prompt")
	}
	var ps struct {
		AllowedTools []string `json:"allowed_tools"`
	}
	if err := json.Unmarshal(policyScope, &ps); err != nil {
		t.Fatal(err)
	}
	if len(ps.AllowedTools) != 2 {
		t.Fatalf("installed agent allowed_tools = %v, want 2 slack tools", ps.AllowedTools)
	}

	// Installing the same template again must not fail outright — a
	// second GitHub PR Reviewer for a second repo is a reasonable thing
	// to want (see Install's own doc comment) — it disambiguates the name
	// instead.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/templates/"+slackID+"/install"))
	if w.Code != http.StatusCreated {
		t.Fatalf("second Install status = %d, body = %s", w.Code, w.Body.String())
	}
	var installedAgain installResponse
	mustUnmarshalData(t, w.Body.Bytes(), &installedAgain)
	if installedAgain.AgentID == installed.AgentID {
		t.Fatal("second Install returned the same agent_id as the first")
	}
}

func TestTemplatesHandler_InstallRequiresOwnerOrAdmin(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	_, clerkUserID := testOrgAndUser(t, systemPool, "member")
	r := testRouter(t, systemPool, appPool, cfg)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/templates"))
	var list []templateSummary
	mustUnmarshalData(t, w.Body.Bytes(), &list)
	if len(list) == 0 {
		t.Fatal("no templates to test against")
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/templates/"+list[0].ID+"/install"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a member installing a template", w.Code)
	}
	var apiErr apiError
	if err := json.Unmarshal(w.Body.Bytes(), &apiErr); err != nil {
		t.Fatal(err)
	}
	if apiErr.Error.Code != "NOT_AUTHORIZED" {
		t.Fatalf("error code = %q, want NOT_AUTHORIZED", apiErr.Error.Code)
	}
}

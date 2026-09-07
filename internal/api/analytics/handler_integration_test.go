//go:build integration

package analytics

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

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

type apiEnvelope struct {
	Data json.RawMessage `json:"data"`
}

// testOrgAndUser builds a fresh org (with a real workflow/run chain, so
// completed_at-scoped runs can be inserted) plus one member user.
func testOrgAndUser(t *testing.T, systemPool *pgxpool.Pool) (orgID pgtype.UUID, clerkUserID string, workflowID pgtype.UUID) {
	t.Helper()
	suffix := randSuffix(t)
	ctx := context.Background()
	clerkUserID = "user_analytics_test_" + suffix

	if err := systemPool.QueryRow(ctx,
		"insert into organizations (clerk_org_id, name, slug) values ($1, 'Analytics Test Org', $2) returning id",
		"org_analytics_test_"+suffix, "analytics-test-"+suffix,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	t.Cleanup(func() { _, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID) })

	if _, err := systemPool.Exec(ctx,
		`insert into users (org_id, clerk_user_id, email) values ($1, $2, 'analytics-test@example.com')`,
		orgID, clerkUserID,
	); err != nil {
		t.Fatalf("insert test user: %v", err)
	}

	var agentID pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into agents (org_id, name, slug, system_prompt) values ($1, 'Analytics Test Agent', $2, 'test') returning id`,
		orgID, "analytics-test-agent-"+suffix,
	).Scan(&agentID); err != nil {
		t.Fatalf("insert test agent: %v", err)
	}

	if err := systemPool.QueryRow(ctx,
		`insert into workflows (org_id, agent_id, name, trigger_type, graph_definition) values ($1, $2, 'Analytics Test Workflow', 'manual', '{}'::jsonb) returning id`,
		orgID, agentID,
	).Scan(&workflowID); err != nil {
		t.Fatalf("insert test workflow: %v", err)
	}

	return orgID, clerkUserID, workflowID
}

func insertCompletedRun(t *testing.T, systemPool *pgxpool.Pool, orgID, workflowID pgtype.UUID, hoursSaved float64, completedAt time.Time) {
	t.Helper()
	if _, err := systemPool.Exec(context.Background(),
		`insert into workflow_runs (workflow_id, org_id, status, hours_saved, completed_at) values ($1, $2, 'completed', $3, $4)`,
		workflowID, orgID, hoursSaved, completedAt,
	); err != nil {
		t.Fatalf("insert completed run: %v", err)
	}
}

// insertRun is the general form insertCompletedRun wraps — status/
// duration/cost are all workflow 14's own agent-performance inputs, none
// of which the hours-saved-only helper above sets.
func insertRun(t *testing.T, systemPool *pgxpool.Pool, orgID, workflowID pgtype.UUID, status string, durationMs int32, costUSD float64) {
	t.Helper()
	if _, err := systemPool.Exec(context.Background(),
		`insert into workflow_runs (workflow_id, org_id, status, duration_ms, cost_so_far_usd) values ($1, $2, $3, $4, $5)`,
		workflowID, orgID, status, durationMs, costUSD,
	); err != nil {
		t.Fatalf("insert run: %v", err)
	}
}

func insertRagSearchAuditLog(t *testing.T, systemPool *pgxpool.Pool, orgID pgtype.UUID, resultCount int, avgRerankScore float64, fromCache bool, createdAt time.Time) {
	t.Helper()
	metadata, err := json.Marshal(map[string]any{
		"result_count": resultCount, "avg_rerank_score": avgRerankScore, "from_cache": fromCache,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := systemPool.Exec(context.Background(),
		`insert into audit_logs (org_id, actor_type, action, metadata_info, created_at) values ($1, 'user', 'rag.search', $2, $3)`,
		orgID, metadata, createdAt,
	); err != nil {
		t.Fatalf("insert rag.search audit log: %v", err)
	}
}

func TestAnalyticsHandler_HoursSaved(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg)

	orgID, clerkUserID, workflowID := testOrgAndUser(t, systemPool)

	now := time.Now().UTC()
	// One run this week, one earlier this month but before this week, one
	// last month entirely — exercises all 3 time windows distinctly.
	insertCompletedRun(t, systemPool, orgID, workflowID, 0.5, now)
	insertCompletedRun(t, systemPool, orgID, workflowID, 1.0, now.AddDate(0, 0, -10))
	insertCompletedRun(t, systemPool, orgID, workflowID, 2.0, now.AddDate(0, -2, 0))

	if _, err := systemPool.Exec(context.Background(),
		"update organizations set total_hours_saved = 3.5 where id = $1", orgID,
	); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/analytics/hours-saved", nil)
	token, err := devtoken.Sign(cfg.DevTokenSecret.Expose(), clerkUserID)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var env apiEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var got hoursSavedResponse
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}

	if got.TotalHoursSaved != 3.5 {
		t.Errorf("TotalHoursSaved = %v, want 3.5", got.TotalHoursSaved)
	}
	if got.EquivalentSalaryUSD != 3.5*50.0 {
		t.Errorf("EquivalentSalaryUSD = %v, want %v", got.EquivalentSalaryUSD, 3.5*50.0)
	}
	// this_month always includes this_week's run(s) too, unless the month
	// boundary and week boundary happen to coincide.
	if got.ThisMonthHoursSaved < got.ThisWeekHoursSaved {
		t.Errorf("ThisMonthHoursSaved (%v) should be >= ThisWeekHoursSaved (%v)", got.ThisMonthHoursSaved, got.ThisWeekHoursSaved)
	}
	if got.ThisWeekHoursSaved != 0.5 {
		t.Errorf("ThisWeekHoursSaved = %v, want 0.5 (only the run from `now` itself)", got.ThisWeekHoursSaved)
	}
}

func TestAnalyticsHandler_HoursSaved_CrossOrgIsolation(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg)

	otherOrgID, _, otherWorkflowID := testOrgAndUser(t, systemPool)
	insertCompletedRun(t, systemPool, otherOrgID, otherWorkflowID, 10.0, time.Now().UTC())
	if _, err := systemPool.Exec(context.Background(), "update organizations set total_hours_saved = 10 where id = $1", otherOrgID); err != nil {
		t.Fatal(err)
	}

	_, clerkUserID, _ := testOrgAndUser(t, systemPool)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/analytics/hours-saved", nil)
	token, err := devtoken.Sign(cfg.DevTokenSecret.Expose(), clerkUserID)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var env apiEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var got hoursSavedResponse
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.TotalHoursSaved != 0 {
		t.Fatalf("TotalHoursSaved = %v, want 0 (another org's hours must not leak in)", got.TotalHoursSaved)
	}
}

func getJSON[T any](t *testing.T, router *gin.Engine, cfg *config.Config, clerkUserID, path string) T {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	token, err := devtoken.Sign(cfg.DevTokenSecret.Expose(), clerkUserID)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200; body = %s", path, rec.Code, rec.Body.String())
	}
	var env apiEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var out T
	if err := json.Unmarshal(env.Data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAnalyticsHandler_AgentPerformance(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg)

	orgID, clerkUserID, workflowID := testOrgAndUser(t, systemPool)
	insertRun(t, systemPool, orgID, workflowID, "completed", 1000, 0.10)
	insertRun(t, systemPool, orgID, workflowID, "completed", 3000, 0.30)
	insertRun(t, systemPool, orgID, workflowID, "failed", 500, 0.05)

	got := getJSON[[]agentPerformanceItem](t, router, cfg, clerkUserID, "/api/v1/analytics/agent-performance")
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	item := got[0]
	if item.TotalRuns != 3 {
		t.Errorf("TotalRuns = %d, want 3", item.TotalRuns)
	}
	if item.FailureCount != 1 {
		t.Errorf("FailureCount = %d, want 1", item.FailureCount)
	}
	// 2 of 3 runs completed.
	wantSuccessRate := 2.0 / 3.0
	if diff := item.SuccessRate - wantSuccessRate; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("SuccessRate = %v, want %v", item.SuccessRate, wantSuccessRate)
	}
	if item.AvgDurationMs != (1000.0+3000.0+500.0)/3.0 {
		t.Errorf("AvgDurationMs = %v, want %v", item.AvgDurationMs, (1000.0+3000.0+500.0)/3.0)
	}
}

func TestAnalyticsHandler_AgentPerformance_CrossOrgIsolation(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg)

	otherOrgID, _, otherWorkflowID := testOrgAndUser(t, systemPool)
	insertRun(t, systemPool, otherOrgID, otherWorkflowID, "completed", 1000, 1.0)

	_, clerkUserID, _ := testOrgAndUser(t, systemPool)

	got := getJSON[[]agentPerformanceItem](t, router, cfg, clerkUserID, "/api/v1/analytics/agent-performance")
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0 (another org's runs must not leak in)", len(got))
	}
}

func TestAnalyticsHandler_RagQuality(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg)

	orgID, clerkUserID, _ := testOrgAndUser(t, systemPool)
	now := time.Now().UTC()
	insertRagSearchAuditLog(t, systemPool, orgID, 5, 0.8, false, now)
	insertRagSearchAuditLog(t, systemPool, orgID, 5, 0.9, true, now)
	insertRagSearchAuditLog(t, systemPool, orgID, 0, 0.0, false, now)
	// Outside the 30-day window — must not affect the aggregate.
	insertRagSearchAuditLog(t, systemPool, orgID, 10, 1.0, true, now.AddDate(0, 0, -45))

	got := getJSON[ragQualityResponse](t, router, cfg, clerkUserID, "/api/v1/analytics/rag-quality")
	if got.TotalSearches != 3 {
		t.Fatalf("TotalSearches = %d, want 3 (the 45-day-old row must be excluded)", got.TotalSearches)
	}
	wantAvgRerank := (0.8 + 0.9 + 0.0) / 3.0
	if diff := got.AvgRerankScore - wantAvgRerank; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("AvgRerankScore = %v, want %v", got.AvgRerankScore, wantAvgRerank)
	}
	wantCacheHitRate := 1.0 / 3.0
	if diff := got.CacheHitRate - wantCacheHitRate; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("CacheHitRate = %v, want %v", got.CacheHitRate, wantCacheHitRate)
	}
	wantAvgChunks := (5.0 + 5.0 + 0.0) / 3.0
	if diff := got.AvgChunksRetrieved - wantAvgChunks; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("AvgChunksRetrieved = %v, want %v", got.AvgChunksRetrieved, wantAvgChunks)
	}
}

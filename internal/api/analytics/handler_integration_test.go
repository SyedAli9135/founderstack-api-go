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

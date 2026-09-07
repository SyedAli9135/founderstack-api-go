//go:build integration

package billing

import (
	"context"
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
	"github.com/founderstack/api/internal/api/response"
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

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{AppEnv: "development", DevTokenSecret: "test-dev-token-secret"}
}

func testRouter(t *testing.T, systemPool, appPool *pgxpool.Pool, cfg *config.Config) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())
	rg := r.Group("/api/v1")
	rg.Use(middleware.RequireAuth(systemPool, cfg))
	NewHandler(appPool).Register(rg)
	return r
}

func testOrgAndUser(t *testing.T, systemPool *pgxpool.Pool) (orgID pgtype.UUID, clerkUserID string) {
	t.Helper()
	suffix := response.NewID()[:12]
	orgClerkID := "org_billing_test_" + suffix
	clerkUserID = "user_billing_test_" + suffix
	ctx := context.Background()

	if err := systemPool.QueryRow(ctx,
		"insert into organizations (clerk_org_id, name, slug) values ($1, 'Billing Test Org', $2) returning id",
		orgClerkID, "billing-test-"+suffix,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	if _, err := systemPool.Exec(ctx,
		`insert into users (org_id, clerk_user_id, email, role) values ($1, $2, 'billing-test@example.com', 'admin')`,
		orgID, clerkUserID,
	); err != nil {
		t.Fatalf("insert test user: %v", err)
	}

	t.Cleanup(func() { _, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID) })
	return orgID, clerkUserID
}

func insertCostLedgerEntry(t *testing.T, systemPool *pgxpool.Pool, orgID pgtype.UUID, agentID pgtype.UUID, costUSD float64, createdAt time.Time) {
	t.Helper()
	if _, err := systemPool.Exec(context.Background(),
		`insert into cost_ledger (org_id, agent_id, cost_type, input_tokens, output_tokens, cached_tokens, estimated_cost_usd, created_at)
		 values ($1, $2, 'llm_inference', 100, 50, 10, $3, $4)`,
		orgID, agentID, costUSD, createdAt,
	); err != nil {
		t.Fatalf("insert cost_ledger entry: %v", err)
	}
}

func authedGet(t *testing.T, router *gin.Engine, cfg *config.Config, clerkUserID, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	token, err := devtoken.Sign(cfg.DevTokenSecret.Expose(), clerkUserID)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestBillingHandler_Usage(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg)

	orgID, clerkUserID := testOrgAndUser(t, systemPool)
	now := time.Now().UTC()

	var agentID pgtype.UUID
	if err := systemPool.QueryRow(context.Background(),
		`insert into agents (org_id, name, slug, system_prompt) values ($1, 'Billing Test Agent', 'billing-test-agent', 'test') returning id`,
		orgID,
	).Scan(&agentID); err != nil {
		t.Fatal(err)
	}

	insertCostLedgerEntry(t, systemPool, orgID, agentID, 1.0, now)
	insertCostLedgerEntry(t, systemPool, orgID, agentID, 2.0, now.AddDate(0, 0, -5))
	// Outside the 30-day window — must not appear anywhere in the response.
	insertCostLedgerEntry(t, systemPool, orgID, agentID, 99.0, now.AddDate(0, 0, -45))

	rec := authedGet(t, router, cfg, clerkUserID, "/api/v1/billing/usage")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data usageResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	got := env.Data
	if got.TotalEstimatedUsd != 3.0 {
		t.Fatalf("TotalEstimatedUsd = %v, want 3.0 (the 45-day-old row must be excluded)", got.TotalEstimatedUsd)
	}
	if len(got.DailyUsage) != 2 {
		t.Fatalf("len(DailyUsage) = %d, want 2 (one bucket per distinct day within the window)", len(got.DailyUsage))
	}
	if len(got.AgentCostShare) != 1 || got.AgentCostShare[0].AgentName != "Billing Test Agent" {
		t.Fatalf("AgentCostShare = %+v, want one entry for Billing Test Agent", got.AgentCostShare)
	}
	if got.AgentCostShare[0].TotalCostUsd != 3.0 {
		t.Errorf("AgentCostShare[0].TotalCostUsd = %v, want 3.0", got.AgentCostShare[0].TotalCostUsd)
	}
}

func TestBillingHandler_Usage_CrossOrgIsolation(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg)

	otherOrgID, _ := testOrgAndUser(t, systemPool)
	insertCostLedgerEntry(t, systemPool, otherOrgID, pgtype.UUID{}, 50.0, time.Now().UTC())

	_, clerkUserID := testOrgAndUser(t, systemPool)

	rec := authedGet(t, router, cfg, clerkUserID, "/api/v1/billing/usage")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data usageResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.TotalEstimatedUsd != 0 {
		t.Fatalf("TotalEstimatedUsd = %v, want 0 — another org's cost_ledger rows must not leak in", env.Data.TotalEstimatedUsd)
	}
}

func TestBillingHandler_Ledger(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg)

	orgID, clerkUserID := testOrgAndUser(t, systemPool)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		insertCostLedgerEntry(t, systemPool, orgID, pgtype.UUID{}, float64(i+1), now.Add(time.Duration(i)*time.Minute))
	}

	rec := authedGet(t, router, cfg, clerkUserID, "/api/v1/billing/ledger?limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data ledgerResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Total != 3 {
		t.Fatalf("Total = %d, want 3", env.Data.Total)
	}
	if len(env.Data.Entries) != 2 {
		t.Fatalf("len(Entries) = %d, want 2 (limit=2)", len(env.Data.Entries))
	}
	// ORDER BY created_at DESC — the most recently inserted (cost 3.0) comes first.
	if env.Data.Entries[0].EstimatedCostUsd != 3.0 {
		t.Errorf("Entries[0].EstimatedCostUsd = %v, want 3.0 (most recent first)", env.Data.Entries[0].EstimatedCostUsd)
	}
}

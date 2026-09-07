//go:build integration

package settings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/response"
)

// testOrgAndUserWithOrgID is testOrgAndUser plus the org's own id — the
// shared helper only returns clerkUserID (8 existing call sites depend on
// that exact signature), but the usage aggregate needs a real org_id to
// insert cost_ledger fixture rows against.
func testOrgAndUserWithOrgID(t *testing.T, systemPool *pgxpool.Pool) (orgID pgtype.UUID, clerkUserID string) {
	t.Helper()
	suffix := response.NewID()[:12]
	orgClerkID := "org_usage_test_" + suffix
	clerkUserID = "user_usage_test_" + suffix
	ctx := context.Background()

	if err := systemPool.QueryRow(ctx,
		"insert into organizations (clerk_org_id, name, slug) values ($1, 'Usage Test Org', $2) returning id",
		orgClerkID, "usage-test-"+suffix,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	if _, err := systemPool.Exec(ctx,
		`insert into users (org_id, clerk_user_id, email, role, can_manage_api_keys) values ($1, $2, 'usage-test@example.com', 'admin', true)`,
		orgID, clerkUserID,
	); err != nil {
		t.Fatalf("insert test user: %v", err)
	}

	t.Cleanup(func() { _, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID) })
	return orgID, clerkUserID
}

func insertCostLedgerEntry(t *testing.T, systemPool *pgxpool.Pool, orgID pgtype.UUID, inputTok, outputTok, cachedTok int32, costUSD float64, createdAt time.Time) {
	t.Helper()
	if _, err := systemPool.Exec(context.Background(),
		`insert into cost_ledger (org_id, cost_type, input_tokens, output_tokens, cached_tokens, estimated_cost_usd, created_at)
		 values ($1, 'llm_inference', $2, $3, $4, $5, $6)`,
		orgID, inputTok, outputTok, cachedTok, costUSD, createdAt,
	); err != nil {
		t.Fatalf("insert cost_ledger entry: %v", err)
	}
}

func TestSettingsAPIKey_Usage(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg, testEncryptionKey(t))

	orgID, clerkUserID := testOrgAndUserWithOrgID(t, systemPool)
	now := time.Now().UTC()
	startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	insertCostLedgerEntry(t, systemPool, orgID, 1000, 500, 200, 0.05, now)
	// Before this calendar month — must not count toward the aggregate.
	insertCostLedgerEntry(t, systemPool, orgID, 9999, 9999, 9999, 99.0, startOfMonth.AddDate(0, 0, -1))

	req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/settings/api-key/usage", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var env struct {
		Data apiKeyUsageResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	got := env.Data
	if got.InputTokens != 1000 || got.OutputTokens != 500 || got.CachedTokens != 200 {
		t.Fatalf("tokens = (%d, %d, %d), want (1000, 500, 200) — the earlier-month row must be excluded",
			got.InputTokens, got.OutputTokens, got.CachedTokens)
	}
	if got.TotalEstimatedUsd != 0.05 {
		t.Errorf("TotalEstimatedUsd = %v, want 0.05", got.TotalEstimatedUsd)
	}
	wantCacheHitRate := 200.0 / (1000.0 + 200.0)
	if diff := got.CacheHitRate - wantCacheHitRate; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("CacheHitRate = %v, want %v", got.CacheHitRate, wantCacheHitRate)
	}
}

func TestSettingsAPIKey_Usage_CrossOrgIsolation(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg, testEncryptionKey(t))

	otherOrgID, _ := testOrgAndUserWithOrgID(t, systemPool)
	insertCostLedgerEntry(t, systemPool, otherOrgID, 5000, 5000, 5000, 10.0, time.Now().UTC())

	_, clerkUserID := testOrgAndUserWithOrgID(t, systemPool)

	req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/settings/api-key/usage", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var env struct {
		Data apiKeyUsageResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.InputTokens != 0 || env.Data.TotalEstimatedUsd != 0 {
		t.Fatalf("usage = %+v, want all-zero — another org's cost_ledger rows must not leak in", env.Data)
	}
}

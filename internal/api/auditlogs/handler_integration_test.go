//go:build integration

package auditlogs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// testOrgAndUserWithRole builds a fresh org + one user of the given role,
// plus one agent (for actor_type='agent' fixture rows to resolve a real
// name against).
func testOrgAndUserWithRole(t *testing.T, systemPool *pgxpool.Pool, role string) (orgID pgtype.UUID, clerkUserID string, userID, agentID pgtype.UUID) {
	t.Helper()
	suffix := response.NewID()[:12]
	ctx := context.Background()
	clerkUserID = "user_auditlogs_test_" + suffix

	if err := systemPool.QueryRow(ctx,
		"insert into organizations (clerk_org_id, name, slug) values ($1, 'Audit Logs Test Org', $2) returning id",
		"org_auditlogs_test_"+suffix, "auditlogs-test-"+suffix,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	t.Cleanup(func() { _, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID) })

	if err := systemPool.QueryRow(ctx,
		`insert into users (org_id, clerk_user_id, email, full_name, role) values ($1, $2, $3, 'Test User', $4) returning id`,
		orgID, clerkUserID, "auditlogs-test-"+suffix+"@example.com", role,
	).Scan(&userID); err != nil {
		t.Fatalf("insert test user: %v", err)
	}

	if err := systemPool.QueryRow(ctx,
		`insert into agents (org_id, name, slug, system_prompt) values ($1, 'Audit Test Agent', $2, 'test') returning id`,
		orgID, "audit-test-agent-"+suffix,
	).Scan(&agentID); err != nil {
		t.Fatalf("insert test agent: %v", err)
	}

	return orgID, clerkUserID, userID, agentID
}

func insertAuditLog(t *testing.T, systemPool *pgxpool.Pool, orgID, actorID pgtype.UUID, actorType, action, status string, createdAt time.Time) {
	t.Helper()
	if _, err := systemPool.Exec(context.Background(),
		`insert into audit_logs (org_id, actor_id, actor_type, action, status, created_at) values ($1, $2, $3, $4, $5, $6)`,
		orgID, actorID, actorType, action, status, createdAt,
	); err != nil {
		t.Fatalf("insert audit log: %v", err)
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

func decodeList(t *testing.T, rec *httptest.ResponseRecorder) auditLogsResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data auditLogsResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, rec.Body.String())
	}
	return env.Data
}

func TestAuditLogsHandler_ListNewestFirstWithResolvedActorNames(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg)

	orgID, clerkUserID, userID, agentID := testOrgAndUserWithRole(t, systemPool, "admin")
	now := time.Now().UTC()
	insertAuditLog(t, systemPool, orgID, userID, "user", "workflow.approval.approved", "success", now.Add(-2*time.Minute))
	insertAuditLog(t, systemPool, orgID, agentID, "agent", "tool.executed", "success", now.Add(-1*time.Minute))

	rec := authedGet(t, router, cfg, clerkUserID, "/api/v1/audit-logs")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	got := decodeList(t, rec)
	if len(got.Entries) != 2 {
		t.Fatalf("len(Entries) = %d, want 2", len(got.Entries))
	}
	// Newest first: tool.executed (agent) was inserted more recently than
	// the approval decision (user).
	if got.Entries[0].Action != "tool.executed" || got.Entries[0].ActorType != "agent" || got.Entries[0].ActorName != "Audit Test Agent" {
		t.Errorf("Entries[0] = %+v, want tool.executed by agent \"Audit Test Agent\"", got.Entries[0])
	}
	if got.Entries[1].Action != "workflow.approval.approved" || got.Entries[1].ActorType != "user" || got.Entries[1].ActorName != "Test User" {
		t.Errorf("Entries[1] = %+v, want workflow.approval.approved by user \"Test User\"", got.Entries[1])
	}
}

func TestAuditLogsHandler_SystemActorFallsBackToSystemName(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg)

	orgID, clerkUserID, _, _ := testOrgAndUserWithRole(t, systemPool, "admin")
	// No matching users/agents row for this actor_id — exactly the
	// 'system' case neither join can resolve.
	insertAuditLog(t, systemPool, orgID, pgtype.UUID{}, "system", "approval.expired", "success", time.Now().UTC())

	rec := authedGet(t, router, cfg, clerkUserID, "/api/v1/audit-logs")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	got := decodeList(t, rec)
	if len(got.Entries) != 1 || got.Entries[0].ActorName != "System" {
		t.Fatalf("Entries = %+v, want exactly 1 entry with ActorName \"System\"", got.Entries)
	}
}

func TestAuditLogsHandler_Filters(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg)

	orgID, clerkUserID, userID, agentID := testOrgAndUserWithRole(t, systemPool, "admin")
	now := time.Now().UTC()
	insertAuditLog(t, systemPool, orgID, userID, "user", "workflow.approval.approved", "success", now)
	insertAuditLog(t, systemPool, orgID, userID, "user", "workflow.approval.rejected", "success", now)
	insertAuditLog(t, systemPool, orgID, agentID, "agent", "tool.executed", "error", now)

	t.Run("actor_type", func(t *testing.T) {
		got := decodeList(t, authedGet(t, router, cfg, clerkUserID, "/api/v1/audit-logs?actor_type=agent"))
		if len(got.Entries) != 1 || got.Entries[0].ActorType != "agent" {
			t.Fatalf("Entries = %+v, want exactly 1 agent entry", got.Entries)
		}
	})

	t.Run("action prefix", func(t *testing.T) {
		got := decodeList(t, authedGet(t, router, cfg, clerkUserID, "/api/v1/audit-logs?action=workflow.approval"))
		if len(got.Entries) != 2 {
			t.Fatalf("len(Entries) = %d, want 2", len(got.Entries))
		}
	})

	t.Run("status", func(t *testing.T) {
		got := decodeList(t, authedGet(t, router, cfg, clerkUserID, "/api/v1/audit-logs?status=error"))
		if len(got.Entries) != 1 || got.Entries[0].Action != "tool.executed" {
			t.Fatalf("Entries = %+v, want exactly the 1 errored tool.executed entry", got.Entries)
		}
	})

	t.Run("date range excludes everything before date_from", func(t *testing.T) {
		future := now.Add(time.Hour).Format(time.RFC3339)
		got := decodeList(t, authedGet(t, router, cfg, clerkUserID, "/api/v1/audit-logs?date_from="+url.QueryEscape(future)))
		if len(got.Entries) != 0 {
			t.Fatalf("Entries = %+v, want 0 (date_from is in the future)", got.Entries)
		}
	})
}

func TestAuditLogsHandler_Pagination(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg)

	orgID, clerkUserID, userID, _ := testOrgAndUserWithRole(t, systemPool, "admin")
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		insertAuditLog(t, systemPool, orgID, userID, "user", "test.action", "success", now.Add(time.Duration(i)*time.Second))
	}

	page1 := decodeList(t, authedGet(t, router, cfg, clerkUserID, "/api/v1/audit-logs?limit=2"))
	if len(page1.Entries) != 2 || page1.NextCursor == nil {
		t.Fatalf("page1 = %+v, want 2 entries + a next_cursor", page1)
	}

	page2 := decodeList(t, authedGet(t, router, cfg, clerkUserID, "/api/v1/audit-logs?limit=2&"+cursorQuery(page1.NextCursor)))
	if len(page2.Entries) != 2 || page2.NextCursor == nil {
		t.Fatalf("page2 = %+v, want 2 entries + a next_cursor", page2)
	}
	if page1.Entries[1].ID == page2.Entries[0].ID {
		t.Fatal("page2's first entry duplicates page1's last entry — cursor is not advancing")
	}

	page3 := decodeList(t, authedGet(t, router, cfg, clerkUserID, "/api/v1/audit-logs?limit=2&"+cursorQuery(page2.NextCursor)))
	if len(page3.Entries) != 1 || page3.NextCursor != nil {
		t.Fatalf("page3 = %+v, want exactly 1 entry and no next_cursor (5 total, 2+2+1)", page3)
	}
}

// cursorQuery URL-encodes a cursor's fields — CreatedAt is RFC3339, whose
// timezone offset contains a literal '+' that Go's query-string parser
// otherwise decodes as a space (application/x-www-form-urlencoded
// convention), corrupting the timestamp. A real bug caught by this test
// failing before this helper existed, not a hypothetical.
func cursorQuery(c *cursor) string {
	v := url.Values{}
	v.Set("cursor_created_at", c.CreatedAt)
	v.Set("cursor_id", c.ID)
	return v.Encode()
}

func TestAuditLogsHandler_MemberAndViewerBlocked(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg)

	for _, role := range []string{"member", "viewer"} {
		t.Run(role, func(t *testing.T) {
			_, clerkUserID, _, _ := testOrgAndUserWithRole(t, systemPool, role)
			rec := authedGet(t, router, cfg, clerkUserID, "/api/v1/audit-logs")
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s: status = %d, want 403; body = %s", role, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAuditLogsHandler_CrossOrgIsolation(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg)

	otherOrgID, _, otherUserID, _ := testOrgAndUserWithRole(t, systemPool, "admin")
	insertAuditLog(t, systemPool, otherOrgID, otherUserID, "user", "some.action", "success", time.Now().UTC())

	_, clerkUserID, _, _ := testOrgAndUserWithRole(t, systemPool, "admin")

	got := decodeList(t, authedGet(t, router, cfg, clerkUserID, "/api/v1/audit-logs"))
	if len(got.Entries) != 0 {
		t.Fatalf("Entries = %+v, want 0 — another org's audit log rows must not leak in", got.Entries)
	}
}

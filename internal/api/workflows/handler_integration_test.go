//go:build integration

package workflows

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
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

func testOrgAndUser(t *testing.T, systemPool *pgxpool.Pool) (orgID, userID pgtype.UUID, clerkUserID string) {
	t.Helper()
	suffix := randSuffix(t)
	clerkUserID = "user_workflows_test_" + suffix
	ctx := context.Background()

	err := systemPool.QueryRow(ctx,
		"insert into organizations (clerk_org_id, name, slug) values ($1, 'Workflows Test Org', $2) returning id",
		"org_workflows_test_"+suffix, "workflows-test-"+suffix,
	).Scan(&orgID)
	if err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	err = systemPool.QueryRow(ctx,
		`insert into users (org_id, clerk_user_id, email) values ($1, $2, 'workflows-test@example.com') returning id`,
		orgID, clerkUserID,
	).Scan(&userID)
	if err != nil {
		t.Fatalf("insert test user: %v", err)
	}

	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID)
	})
	return orgID, userID, clerkUserID
}

// testAgent inserts a minimal active agent directly (workflow 7's own
// validation isn't what's under test here) for workflows to reference.
func testAgent(t *testing.T, appPool *pgxpool.Pool, orgID, userID pgtype.UUID, name string) pgtype.UUID {
	t.Helper()
	var agentID pgtype.UUID
	err := tenant.WithTx(context.Background(), appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		policyScope := []byte(`{"allowed_tools":["stripe.get_mrr"]}`)
		allowedServers := []byte(`["stripe"]`)
		systemPrompt := "You are a test agent used only to exercise workflow 8's integration tests."
		row, err := q.InsertAgent(ctx, dbgen.InsertAgentParams{
			OrgID: orgID, Name: name, Slug: "test-agent-" + randSuffix(t), AgentType: "specialist",
			SystemPrompt: systemPrompt, PolicyScope: policyScope, AllowedMcpServers: allowedServers,
			CreatedBy: userID,
		})
		if err != nil {
			return err
		}
		agentID = row.ID
		return nil
	})
	if err != nil {
		t.Fatalf("insert test agent: %v", err)
	}
	return agentID
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

	h := NewHandler(appPool)
	authed := r.Group("/api/v1")
	authed.Use(middleware.RequireAuth(systemPool, cfg))
	h.Register(authed)

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
			t.Fatal(err)
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

type apiEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func TestWorkflowsHandler_FullLifecycle(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	orgID, userID, clerkUserID := testOrgAndUser(t, systemPool)
	agentID := testAgent(t, appPool, orgID, userID, "Test Agent")
	router := testRouter(t, systemPool, appPool, cfg)

	t.Run("unauthenticated list is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("create rejects an unknown agent_id", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/workflows", map[string]any{
			"agent_id": "00000000-0000-0000-0000-000000000000", "name": "X", "trigger_type": "manual",
		})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("create rejects scheduled without a cron_expression", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/workflows", map[string]any{
			"agent_id": agentID.String(), "name": "X", "trigger_type": "scheduled",
		})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
		var env apiEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if env.Error.Code != "CRON_EXPRESSION_REQUIRED" {
			t.Fatalf("error code = %q, want CRON_EXPRESSION_REQUIRED", env.Error.Code)
		}
	})

	t.Run("create rejects an invalid cron_expression", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/workflows", map[string]any{
			"agent_id": agentID.String(), "name": "X", "trigger_type": "scheduled", "cron_expression": "nonsense",
		})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	var workflowID string
	t.Run("create succeeds for a scheduled workflow and computes next_run_at", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/workflows", map[string]any{
			"agent_id": agentID.String(), "name": "Weekly Digest", "trigger_type": "scheduled",
			"cron_expression": "0 9 * * 1", "requires_approval": true,
			"task_input_template": "Summarize the week.", "estimated_manual_minutes": 20,
		})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
		}
		var env apiEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		var got workflowView
		if err := json.Unmarshal(env.Data, &got); err != nil {
			t.Fatal(err)
		}
		if got.ID == "" || got.AgentName != "Test Agent" || got.NextRunAt == nil ||
			got.CronExpression == nil || *got.CronExpression != "0 9 * * 1" ||
			!got.RequiresApproval || !got.IsActive {
			t.Fatalf("unexpected created workflow: %+v", got)
		}
		workflowID = got.ID
	})

	t.Run("agent's workflow_count reflects the new workflow", func(t *testing.T) {
		var count int64
		err := tenant.WithTx(context.Background(), appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
			row, err := q.GetAgent(ctx, dbgen.GetAgentParams{OrgID: orgID, ID: agentID})
			count = row.WorkflowCount
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("agent workflow_count = %d, want 1", count)
		}
	})

	t.Run("get returns the created workflow", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/workflows/"+workflowID, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("get unknown id is 404", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/workflows/00000000-0000-0000-0000-000000000000", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("pause sets is_active=false but keeps the workflow in the list", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPatch, "/api/v1/workflows/"+workflowID, map[string]any{"is_active": false})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		var env apiEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		var got workflowView
		if err := json.Unmarshal(env.Data, &got); err != nil {
			t.Fatal(err)
		}
		if got.IsActive {
			t.Fatal("is_active still true after pause")
		}

		listReq := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/workflows", nil)
		listRec := httptest.NewRecorder()
		router.ServeHTTP(listRec, listReq)
		var listEnv apiEnvelope
		if err := json.Unmarshal(listRec.Body.Bytes(), &listEnv); err != nil {
			t.Fatal(err)
		}
		var list []workflowView
		if err := json.Unmarshal(listEnv.Data, &list); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, w := range list {
			if w.ID == workflowID {
				found = true
			}
		}
		if !found {
			t.Fatal("paused workflow was excluded from the list — pause must not hide it, unlike agents/documents")
		}
	})

	t.Run("resume sets is_active=true again", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPatch, "/api/v1/workflows/"+workflowID, map[string]any{"is_active": true})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var env apiEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		var got workflowView
		if err := json.Unmarshal(env.Data, &got); err != nil {
			t.Fatal(err)
		}
		if !got.IsActive {
			t.Fatal("is_active still false after resume")
		}
	})

	t.Run("switching trigger_type to manual clears cron_expression and next_run_at", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPatch, "/api/v1/workflows/"+workflowID, map[string]any{"trigger_type": "manual"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		var env apiEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		var got workflowView
		if err := json.Unmarshal(env.Data, &got); err != nil {
			t.Fatal(err)
		}
		if got.TriggerType != "manual" || got.CronExpression != nil || got.NextRunAt != nil {
			t.Fatalf("switch to manual didn't clear schedule fields: %+v", got)
		}
	})

	t.Run("switching a second workflow to scheduled without a cron in the same request is rejected", func(t *testing.T) {
		createReq := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/workflows", map[string]any{
			"agent_id": agentID.String(), "name": "Second Workflow", "trigger_type": "manual",
		})
		createRec := httptest.NewRecorder()
		router.ServeHTTP(createRec, createReq)
		var env apiEnvelope
		if err := json.Unmarshal(createRec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		var created workflowView
		if err := json.Unmarshal(env.Data, &created); err != nil {
			t.Fatal(err)
		}

		req := authedRequest(t, cfg, clerkUserID, http.MethodPatch, "/api/v1/workflows/"+created.ID, map[string]any{"trigger_type": "scheduled"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("run now queues a pending workflow_runs row", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/workflows/"+workflowID+"/run", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
		}
		var env apiEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		var got struct {
			RunID  string `json:"run_id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(env.Data, &got); err != nil {
			t.Fatal(err)
		}
		if got.RunID == "" || got.Status != "pending" {
			t.Fatalf("unexpected run response: %+v", got)
		}

		runUUID, err := uuid.Parse(got.RunID)
		if err != nil {
			t.Fatal(err)
		}
		var dbStatus string
		err = tenant.WithTx(context.Background(), appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
			run, err := q.GetWorkflowRun(ctx, dbgen.GetWorkflowRunParams{OrgID: orgID, ID: pgtype.UUID{Bytes: runUUID, Valid: true}})
			dbStatus = run.Status
			return err
		})
		if err != nil {
			t.Fatalf("workflow_runs row not found: %v", err)
		}
		if dbStatus != "pending" {
			t.Fatalf("workflow_runs.status = %q, want pending", dbStatus)
		}
	})

	t.Run("run now on unknown id is 404", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/workflows/00000000-0000-0000-0000-000000000000/run", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("delete pauses the workflow (same as is_active=false)", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodDelete, "/api/v1/workflows/"+workflowID, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}

		getReq := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/workflows/"+workflowID, nil)
		getRec := httptest.NewRecorder()
		router.ServeHTTP(getRec, getReq)
		if getRec.Code != http.StatusOK {
			t.Fatalf("get after delete: status = %d, want 200 (delete is a pause, not a hard delete)", getRec.Code)
		}
		var env apiEnvelope
		if err := json.Unmarshal(getRec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		var got workflowView
		if err := json.Unmarshal(env.Data, &got); err != nil {
			t.Fatal(err)
		}
		if got.IsActive {
			t.Fatal("workflow still active after delete")
		}
	})

	t.Run("delete on unknown id is 404", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodDelete, "/api/v1/workflows/00000000-0000-0000-0000-000000000000", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

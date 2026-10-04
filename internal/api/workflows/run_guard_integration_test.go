//go:build integration

package workflows

import (
	"context"
	"github.com/founderstack/api/internal/core/graph"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func runCount(t *testing.T, pool *pgxpool.Pool, wfID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "select count(*) from workflow_runs where workflow_id = $1", wfID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWorkflowsHandler_Run_RefusesPausedWorkflowAndDeletedAgent(t *testing.T) {
	appPool, systemPool, cfg := testAppPool(t), testSystemPool(t), testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg)
	orgID, userID, clerkUserID := testOrgAndUser(t, systemPool)
	agentID := testAgent(t, appPool, orgID, userID, "Guard Agent")
	testBYOKKey(t, systemPool, orgID)
	wfID := createTestWorkflow(t, cfg, router, clerkUserID, agentID)

	run := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/workflows/"+wfID+"/run", nil))
		return rec
	}

	if _, err := systemPool.Exec(context.Background(), "update workflows set is_active = false where id = $1", wfID); err != nil {
		t.Fatal(err)
	}
	if rec := run(); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "WORKFLOW_INACTIVE") {
		t.Fatalf("paused workflow: got (%d, %s), want 409 WORKFLOW_INACTIVE", rec.Code, rec.Body.String())
	}

	if _, err := systemPool.Exec(context.Background(), "update workflows set is_active = true where id = $1", wfID); err != nil {
		t.Fatal(err)
	}
	if _, err := systemPool.Exec(context.Background(), "update agents set is_active = false where id = $1", agentID); err != nil {
		t.Fatal(err)
	}
	if rec := run(); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "AGENT_INACTIVE") {
		t.Fatalf("deleted agent: got (%d, %s), want 409 AGENT_INACTIVE", rec.Code, rec.Body.String())
	}

	if n := runCount(t, systemPool, wfID); n != 0 {
		t.Fatalf("%d runs were queued by refused requests, want 0", n)
	}

	if _, err := systemPool.Exec(context.Background(), "update agents set is_active = true where id = $1", agentID); err != nil {
		t.Fatal(err)
	}
	if rec := run(); rec.Code != http.StatusAccepted {
		t.Fatalf("active workflow and agent: status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
}

func TestWorkflowsHandler_Run_CapsConcurrentRunsPerOrg(t *testing.T) {
	appPool, systemPool, cfg := testAppPool(t), testSystemPool(t), testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg)
	orgID, userID, clerkUserID := testOrgAndUser(t, systemPool)
	agentID := testAgent(t, appPool, orgID, userID, "Cap Agent")
	testBYOKKey(t, systemPool, orgID)
	wfID := createTestWorkflow(t, cfg, router, clerkUserID, agentID)

	for i := 0; i < graph.MaxActiveRunsPerOrg; i++ {
		if _, err := systemPool.Exec(context.Background(),
			"insert into workflow_runs (workflow_id, org_id, triggered_by, status) values ($1, $2, $3, 'running')", wfID, orgID, userID); err != nil {
			t.Fatal(err)
		}
	}
	run := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/workflows/"+wfID+"/run", nil))
		return rec
	}
	if rec := run(); rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "TOO_MANY_ACTIVE_RUNS") {
		t.Fatalf("at the cap: got (%d, %s), want 429 TOO_MANY_ACTIVE_RUNS", rec.Code, rec.Body.String())
	}
	if n := runCount(t, systemPool, wfID); n != graph.MaxActiveRunsPerOrg {
		t.Fatalf("run rows = %d, want %d (the refused run must not be queued)", n, graph.MaxActiveRunsPerOrg)
	}

	if _, err := systemPool.Exec(context.Background(),
		"update workflow_runs set status = 'completed' where id = (select id from workflow_runs where workflow_id = $1 limit 1)", wfID); err != nil {
		t.Fatal(err)
	}
	if rec := run(); rec.Code != http.StatusAccepted {
		t.Fatalf("below the cap: status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
}

func TestWorkflowsHandler_RejectsOutOfRangeFields(t *testing.T) {
	appPool, systemPool, cfg := testAppPool(t), testSystemPool(t), testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg)
	orgID, userID, clerkUserID := testOrgAndUser(t, systemPool)
	agentID := testAgent(t, appPool, orgID, userID, "Fields Agent")
	wfID := createTestWorkflow(t, cfg, router, clerkUserID, agentID)

	base := func() map[string]any {
		return map[string]any{"agent_id": agentID.String(), "name": "Field Check", "trigger_type": "manual"}
	}
	create := func(key string, value any) (int, string) {
		b := base()
		b[key] = value
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/workflows", b))
		return rec.Code, rec.Body.String()
	}
	for _, tc := range []struct {
		name  string
		key   string
		value any
		code  string
	}{
		{"name over 255", "name", strings.Repeat("n", 256), "INVALID_WORKFLOW_NAME"},
		{"blank name", "name", " ", "INVALID_WORKFLOW_NAME"},
		{"description over 2000", "description", strings.Repeat("d", 2001), "DESCRIPTION_TOO_LONG"},
		{"task template over 20000", "task_input_template", strings.Repeat("t", maxTaskTemplateLen+1), "TASK_TEMPLATE_TOO_LONG"},
		{"negative manual minutes", "estimated_manual_minutes", -5, "INVALID_MANUAL_MINUTES"},
		{"absurd manual minutes", "estimated_manual_minutes", 2000000000, "INVALID_MANUAL_MINUTES"},
	} {
		if status, body := create(tc.key, tc.value); status != http.StatusBadRequest || !strings.Contains(body, tc.code) {
			t.Errorf("create, %s: got (%d, %s), want 400 %s", tc.name, status, body, tc.code)
		}
	}

	for name, body := range map[string]map[string]any{
		"blank name":       {"name": ""},
		"negative minutes": {"estimated_manual_minutes": -1},
		"long template":    {"task_input_template": strings.Repeat("t", maxTaskTemplateLen+1)},
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, authedRequest(t, cfg, clerkUserID, http.MethodPatch, "/api/v1/workflows/"+wfID, body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("update, %s: status = %d, want 400; body = %s", name, rec.Code, rec.Body.String())
		}
	}
}

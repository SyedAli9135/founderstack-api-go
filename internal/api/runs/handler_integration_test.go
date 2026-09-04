//go:build integration

package runs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/core/graph"
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

func testRouter(t *testing.T, systemPool, appPool *pgxpool.Pool, cfg *config.Config, engine *graph.Engine) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())

	authed := r.Group("/api/v1")
	authed.Use(middleware.RequireAuth(systemPool, cfg))
	NewHandler(appPool, engine).Register(authed)

	return r
}

func authedRequest(t *testing.T, cfg *config.Config, clerkUserID, method, path string) *http.Request {
	t.Helper()
	token, err := devtoken.Sign(cfg.DevTokenSecret.Expose(), clerkUserID)
	if err != nil {
		t.Fatalf("sign dev token: %v", err)
	}
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

type apiEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// testOrgUserAgentWorkflowRun builds the full fixture chain a run row
// needs, and directly SQL-UPDATEs the row into whatever status/output
// the test wants — this package tests observability endpoints, not the
// engine itself (that's internal/core/graph's own test suite), so
// fixtures go straight to the desired end state rather than running a
// real engine pass.
func testOrgUserAgentWorkflowRun(t *testing.T, systemPool *pgxpool.Pool, status string) (orgID pgtype.UUID, clerkUserID string, runID pgtype.UUID) {
	t.Helper()
	suffix := randSuffix(t)
	ctx := context.Background()
	clerkUserID = "user_runs_test_" + suffix

	if err := systemPool.QueryRow(ctx,
		"insert into organizations (clerk_org_id, name, slug) values ($1, 'Runs Test Org', $2) returning id",
		"org_runs_test_"+suffix, "runs-test-"+suffix,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	t.Cleanup(func() { _, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID) })

	var userID pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into users (org_id, clerk_user_id, email) values ($1, $2, 'runs-test@example.com') returning id`,
		orgID, clerkUserID,
	).Scan(&userID); err != nil {
		t.Fatalf("insert test user: %v", err)
	}

	var agentID pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into agents (org_id, name, slug, system_prompt) values ($1, 'Runs Test Agent', $2, 'test') returning id`,
		orgID, "runs-test-agent-"+suffix,
	).Scan(&agentID); err != nil {
		t.Fatalf("insert test agent: %v", err)
	}

	var workflowID pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into workflows (org_id, agent_id, name, trigger_type, graph_definition) values ($1, $2, 'Runs Test Workflow', 'manual', '{}'::jsonb) returning id`,
		orgID, agentID,
	).Scan(&workflowID); err != nil {
		t.Fatalf("insert test workflow: %v", err)
	}

	if err := systemPool.QueryRow(ctx,
		`insert into workflow_runs (workflow_id, org_id, triggered_by, status) values ($1, $2, $3, $4) returning id`,
		workflowID, orgID, userID, status,
	).Scan(&runID); err != nil {
		t.Fatalf("insert test workflow_run: %v", err)
	}

	return orgID, clerkUserID, runID
}

func TestRunsHandler_List(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	engine := graph.NewEngine(appPool)
	router := testRouter(t, systemPool, appPool, cfg, engine)

	orgID, clerkUserID, runID := testOrgUserAgentWorkflowRun(t, systemPool, "completed")
	_, _, otherRunID := testOrgUserAgentWorkflowRun(t, systemPool, "failed")
	// A second org's run must never appear in the first org's list.
	_, _, _ = testOrgUserAgentWorkflowRun(t, systemPool, "completed")
	_ = otherRunID
	_ = orgID

	req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/runs")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var env apiEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Runs []runSummary `json:"runs"`
	}
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Runs) != 1 {
		t.Fatalf("Runs = %d, want exactly 1 (cross-org isolation)", len(got.Runs))
	}
	if got.Runs[0].ID != runID.String() {
		t.Fatalf("Runs[0].ID = %q, want %q", got.Runs[0].ID, runID.String())
	}
}

func TestRunsHandler_List_StatusFilter(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	engine := graph.NewEngine(appPool)
	router := testRouter(t, systemPool, appPool, cfg, engine)

	_, clerkUserID, runID := testOrgUserAgentWorkflowRun(t, systemPool, "completed")

	req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/runs?status=completed")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var env apiEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	var got struct {
		Runs []runSummary `json:"runs"`
	}
	_ = json.Unmarshal(env.Data, &got)
	if len(got.Runs) != 1 || got.Runs[0].ID != runID.String() {
		t.Fatalf("status=completed filter: got %+v", got.Runs)
	}

	req2 := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/runs?status=failed")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	var env2 apiEnvelope
	_ = json.Unmarshal(rec2.Body.Bytes(), &env2)
	var got2 struct {
		Runs []runSummary `json:"runs"`
	}
	_ = json.Unmarshal(env2.Data, &got2)
	if len(got2.Runs) != 0 {
		t.Fatalf("status=failed filter should exclude the completed run, got %+v", got2.Runs)
	}
}

func TestRunsHandler_Get(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	engine := graph.NewEngine(appPool)
	router := testRouter(t, systemPool, appPool, cfg, engine)

	_, clerkUserID, runID := testOrgUserAgentWorkflowRun(t, systemPool, "completed")
	if _, err := systemPool.Exec(context.Background(),
		"update workflow_runs set output = 'the answer', input_tokens = 100, output_tokens = 20 where id = $1", runID,
	); err != nil {
		t.Fatal(err)
	}

	req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/runs/"+runID.String())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var env apiEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var got runDetail
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Output == nil || *got.Output != "the answer" {
		t.Fatalf("Output = %v, want %q", got.Output, "the answer")
	}
	if got.InputTokens != 100 || got.OutputTokens != 20 {
		t.Fatalf("tokens = (%d, %d), want (100, 20)", got.InputTokens, got.OutputTokens)
	}
}

func TestRunsHandler_Get_NotFound(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	engine := graph.NewEngine(appPool)
	router := testRouter(t, systemPool, appPool, cfg, engine)

	_, clerkUserID, _ := testOrgUserAgentWorkflowRun(t, systemPool, "completed")

	req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/runs/00000000-0000-0000-0000-000000000000")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRunsHandler_Get_CrossOrgIsolation(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	engine := graph.NewEngine(appPool)
	router := testRouter(t, systemPool, appPool, cfg, engine)

	_, _, otherOrgRunID := testOrgUserAgentWorkflowRun(t, systemPool, "completed")
	_, clerkUserID, _ := testOrgUserAgentWorkflowRun(t, systemPool, "completed")

	req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/runs/"+otherOrgRunID.String())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (another org's run must not be visible)", rec.Code)
	}
}

func TestRunsHandler_Steps(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	engine := graph.NewEngine(appPool)
	router := testRouter(t, systemPool, appPool, cfg, engine)

	_, clerkUserID, runID := testOrgUserAgentWorkflowRun(t, systemPool, "completed")
	if _, err := systemPool.Exec(context.Background(),
		`insert into workflow_steps (run_id, node_name, step_type, agent_name, input_data, output_data, input_tokens, output_tokens, duration_ms, status)
		 values ($1, 'planner', 'planning', 'Test Agent', '{"input":"do it"}'::jsonb, null, null, null, 5, 'completed'),
		        ($1, 'executor', 'tool_call', 'Test Agent', '{"tool":"fake.get_data"}'::jsonb, '{"result":"ok"}'::jsonb, null, null, 42, 'completed')`,
		runID,
	); err != nil {
		t.Fatal(err)
	}

	req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/runs/"+runID.String()+"/steps")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var env apiEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Steps []workflowStep `json:"steps"`
	}
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Steps) != 2 {
		t.Fatalf("Steps = %d, want 2; got %+v", len(got.Steps), got.Steps)
	}
	if got.Steps[0].NodeName != "planner" || got.Steps[0].StepType != "planning" {
		t.Fatalf("Steps[0] = %+v, want planner/planning first (ordered by created_at)", got.Steps[0])
	}
	if got.Steps[1].NodeName != "executor" || got.Steps[1].StepType != "tool_call" {
		t.Fatalf("Steps[1] = %+v, want executor/tool_call second", got.Steps[1])
	}
	if got.Steps[1].AgentName == nil || *got.Steps[1].AgentName != "Test Agent" {
		t.Fatalf("Steps[1].AgentName = %v, want %q", got.Steps[1].AgentName, "Test Agent")
	}
}

func TestRunsHandler_Steps_NotFound(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	engine := graph.NewEngine(appPool)
	router := testRouter(t, systemPool, appPool, cfg, engine)

	_, clerkUserID, _ := testOrgUserAgentWorkflowRun(t, systemPool, "completed")

	req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/runs/00000000-0000-0000-0000-000000000000/steps")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRunsHandler_Steps_CrossOrgIsolation(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	engine := graph.NewEngine(appPool)
	router := testRouter(t, systemPool, appPool, cfg, engine)

	_, _, otherOrgRunID := testOrgUserAgentWorkflowRun(t, systemPool, "completed")
	_, clerkUserID, _ := testOrgUserAgentWorkflowRun(t, systemPool, "completed")

	req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/runs/"+otherOrgRunID.String()+"/steps")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (another org's run must not be visible)", rec.Code)
	}
}

func TestRunsHandler_Cost(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	engine := graph.NewEngine(appPool)
	router := testRouter(t, systemPool, appPool, cfg, engine)

	orgID, clerkUserID, runID := testOrgUserAgentWorkflowRun(t, systemPool, "completed")
	if _, err := systemPool.Exec(context.Background(),
		`insert into cost_ledger (org_id, run_id, cost_type, model, input_tokens, output_tokens, estimated_cost_usd)
		 values ($1, $2, 'llm_inference', 'claude-sonnet-4-5', 100, 20, 0.005),
		        ($1, $2, 'llm_inference', 'claude-sonnet-4-5', 50, 10, 0.0025),
		        ($1, $2, 'tool_call', 'fake.get_data', 0, 0, 0)`,
		orgID, runID,
	); err != nil {
		t.Fatal(err)
	}

	req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/runs/"+runID.String()+"/cost")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var env apiEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Items    []costBreakdownItem `json:"items"`
		TotalUSD float64             `json:"total_usd"`
	}
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("Items = %d, want 2 (llm_inference, tool_call); got %+v", len(got.Items), got.Items)
	}
	if diff := got.TotalUSD - 0.0075; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("TotalUSD = %v, want 0.0075", got.TotalUSD)
	}
	for _, item := range got.Items {
		if item.CostType == "llm_inference" {
			if item.InputTokens != 150 || item.OutputTokens != 30 {
				t.Fatalf("llm_inference tokens = (%d, %d), want (150, 30)", item.InputTokens, item.OutputTokens)
			}
		}
	}
}

func TestRunsHandler_Cost_NotFound(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	engine := graph.NewEngine(appPool)
	router := testRouter(t, systemPool, appPool, cfg, engine)

	_, clerkUserID, _ := testOrgUserAgentWorkflowRun(t, systemPool, "completed")

	req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/runs/00000000-0000-0000-0000-000000000000/cost")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRunsHandler_Cancel_NotInFlight(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	engine := graph.NewEngine(appPool) // fresh engine, nothing registered in its cancel map
	router := testRouter(t, systemPool, appPool, cfg, engine)

	_, clerkUserID, runID := testOrgUserAgentWorkflowRun(t, systemPool, "completed")

	req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/runs/"+runID.String()+"/cancel")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
}

func TestRunsHandler_Cancel_InFlight(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	engine := graph.NewEngine(appPool)
	router := testRouter(t, systemPool, appPool, cfg, engine)

	_, clerkUserID, runID := testOrgUserAgentWorkflowRun(t, systemPool, "pending")
	runUUID := uuid.UUID(runID.Bytes)

	// Register runUUID in the engine's in-memory cancel map by actually
	// running it — a node that blocks on ctx.Done(), same pattern as
	// internal/core/graph's own TestEngine_CancelStopsAnInFlightRun.
	nodeStarted := make(chan struct{})
	nodes := graph.Nodes{
		"executor": func(ctx context.Context, s *graph.RunState) (graph.NodeName, error) {
			close(nodeStarted)
			<-ctx.Done()
			return "", ctx.Err()
		},
	}
	go func() {
		orgUUID := uuid.UUID(pgtypeOrgID(t, systemPool, runID).Bytes)
		state := &graph.RunState{OrgID: orgUUID, WorkflowRunID: runUUID}
		_ = engine.Run(context.Background(), nodes, state, "executor")
	}()
	<-nodeStarted

	req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/runs/"+runID.String()+"/cancel")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		if err := systemPool.QueryRow(context.Background(), "select status from workflow_runs where id = $1", runID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "cancelled" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", status)
	}
}

func pgtypeOrgID(t *testing.T, systemPool *pgxpool.Pool, runID pgtype.UUID) pgtype.UUID {
	t.Helper()
	var orgID pgtype.UUID
	if err := systemPool.QueryRow(context.Background(), "select org_id from workflow_runs where id = $1", runID).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	return orgID
}

func TestRunsHandler_Stream(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	engine := graph.NewEngine(appPool)
	router := testRouter(t, systemPool, appPool, cfg, engine)

	_, clerkUserID, runID := testOrgUserAgentWorkflowRun(t, systemPool, "running")
	runUUID := uuid.UUID(runID.Bytes)

	// gin.Context.Stream requires http.CloseNotifier, which
	// httptest.ResponseRecorder doesn't implement (it panics inside
	// gin — confirmed by actually running this against a recorder first,
	// not assumed) — this one test needs a real listening server, unlike
	// every other handler test in this file.
	srv := httptest.NewServer(router)
	defer srv.Close()

	token, err := devtoken.Sign(cfg.DevTokenSecret.Expose(), clerkUserID)
	if err != nil {
		t.Fatalf("sign dev token: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/runs/"+runID.String()+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	stop := make(chan struct{})
	defer close(stop)
	// Publish is a no-op with no subscriber yet — keep retrying until the
	// handler's own Subscribe call (which happens after a DB round trip)
	// catches one, rather than guessing a fixed sleep duration.
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			engine.Bus.Publish(graph.Event{Type: graph.EventComplete, RunID: runUUID, Data: "all done"})
			time.Sleep(10 * time.Millisecond)
		}
	}()

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream body: %v", err)
	}
	if !strings.Contains(string(body), "event: complete") {
		t.Fatalf("body = %q, want an %q SSE event", body, "complete")
	}
	if !strings.Contains(string(body), "all done") {
		t.Fatalf("body = %q, want it to contain the event's Data payload", body)
	}
}

func TestRunsHandler_Stream_NotFoundForUnknownRun(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	engine := graph.NewEngine(appPool)
	router := testRouter(t, systemPool, appPool, cfg, engine)

	_, clerkUserID, _ := testOrgUserAgentWorkflowRun(t, systemPool, "completed")

	req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/runs/00000000-0000-0000-0000-000000000000/stream")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

//go:build integration

package teams

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	a2aapi "github.com/founderstack/api/internal/api/a2a"
	"github.com/founderstack/api/internal/api/middleware"
	runsapi "github.com/founderstack/api/internal/api/runs"
	"github.com/founderstack/api/internal/config"
	corea2a "github.com/founderstack/api/internal/core/a2a"
	"github.com/founderstack/api/internal/core/graph"
	"github.com/founderstack/api/internal/core/llm"
	coremcp "github.com/founderstack/api/internal/core/mcp"
	"github.com/founderstack/api/internal/pkg/devtoken"
	"github.com/founderstack/api/internal/pkg/vault"
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
	return &config.Config{AppEnv: "development", DevTokenSecret: "test-dev-token-secret", A2ATaskTokenSecret: "test-a2a-task-token-secret"}
}

// teamTestFixture is the org/user/orchestrator-agent/2-specialist-agent
// chain every test in this file needs — 3 agents, each with a distinct
// `model` string, is deliberate: modelKeyedResolver (below) dispatches a
// separate MockChatClient per model, since decompose/synthesis (the
// orchestrator's own calls) and each specialist's own call must return
// different canned responses and, for the 2 specialists, happen
// concurrently (errgroup fan-out) against what would otherwise be one
// shared, non-concurrency-safe llm.MockChatClient instance.
type teamTestFixture struct {
	userClerkID                             string
	orgPg, orchestratorID, financeID, opsID pgtype.UUID
}

func newTeamTestFixture(t *testing.T, systemPool *pgxpool.Pool) teamTestFixture {
	t.Helper()
	ctx := context.Background()
	suffix := randSuffix(t)
	fx := teamTestFixture{userClerkID: "user_teams_test_" + suffix}

	if err := systemPool.QueryRow(ctx,
		"insert into organizations (clerk_org_id, name, slug, llm_provider) values ($1, 'Teams Test Org', $2, 'anthropic') returning id",
		"org_teams_test_"+suffix, "teams-test-"+suffix,
	).Scan(&fx.orgPg); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", fx.orgPg)
	})

	if _, err := systemPool.Exec(ctx,
		`insert into users (org_id, clerk_user_id, email, role) values ($1, $2, 'teams-test@example.com', 'owner')`,
		fx.orgPg, fx.userClerkID,
	); err != nil {
		t.Fatalf("insert test user: %v", err)
	}

	insertAgent := func(name, slug, model string) pgtype.UUID {
		var id pgtype.UUID
		if err := systemPool.QueryRow(ctx,
			`insert into agents (org_id, name, slug, system_prompt, model, policy_scope) values ($1, $2, $3, 'You are a test agent.', $4, '{"allowed_tools":[]}'::jsonb) returning id`,
			fx.orgPg, name, slug+"-"+suffix, model,
		).Scan(&id); err != nil {
			t.Fatalf("insert test agent %s: %v", name, err)
		}
		return id
	}
	fx.orchestratorID = insertAgent("Orchestrator", "orchestrator", "mock-orchestrator-"+suffix)
	fx.financeID = insertAgent("Finance Agent", "finance-agent", "mock-finance-"+suffix)
	fx.opsID = insertAgent("Ops Agent", "ops-agent", "mock-ops-"+suffix)

	// Launcher.Preflight (called synchronously by POST /teams/{id}/run,
	// same as POST /workflows/{id}/run) requires an active BYOK key to
	// exist — its content is never actually decrypted in this test
	// (modelKeyedResolver bypasses llm.ResolveChatClient entirely), so any
	// valid-length key material satisfies it.
	encKey := make([]byte, 32)
	if _, err := rand.Read(encKey); err != nil {
		t.Fatal(err)
	}
	encrypted, err := vault.Encrypt("fake-key-material", encKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := systemPool.Exec(ctx,
		`insert into api_key_registry (org_id, provider, key_prefix, encrypted_key, kms_key_id, is_valid)
		 values ($1, 'anthropic', 'fake-...', $2, 'local-aes-gcm', true)`,
		fx.orgPg, encrypted,
	); err != nil {
		t.Fatalf("insert test byok key: %v", err)
	}

	return fx
}

// modelKeyedResolver returns a ChatClientResolver dispatching by the
// agent's own `model` string — see teamTestFixture's doc comment for why
// a single shared MockChatClient can't stand in for every call here.
func modelKeyedResolver(byModel map[string]llm.ChatClient) graph.ChatClientResolver {
	return func(ctx context.Context, appPool *pgxpool.Pool, encryptionKey []byte, orgID pgtype.UUID, provider llm.ProviderID, model string) (llm.ChatClient, error) {
		if c, ok := byModel[model]; ok {
			return c, nil
		}
		return nil, fmt.Errorf("modelKeyedResolver: no mock client registered for model %q", model)
	}
}

// buildTeamTestServer wires one real gin.Engine hosting both this
// package's founder-facing routes (teams, RequireAuth-gated) and
// internal/api/a2a's tasks/send route (ungated — see its own package
// doc), all sharing one graph.Launcher/graph.Engine — the same topology
// cmd/api/main.go wires in production, just served from an
// httptest.Server instead of a real listener. The orchestrator's
// A2AClient dispatches back to this same server's own URL, a genuine
// loopback HTTP round trip end to end, not an in-process shortcut.
func buildTeamTestServer(t *testing.T, appPool, systemPool *pgxpool.Pool, cfg *config.Config, byModel map[string]llm.ChatClient) (*httptest.Server, *graph.Launcher) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	engine := graph.NewEngine(appPool)
	registry, err := coremcp.NewRegistry(context.Background(), map[string]*gomcp.Server{})
	if err != nil {
		t.Fatalf("build empty mcp registry: %v", err)
	}
	launcher := graph.NewLauncherWithResolver(engine, appPool, nil, registry, nil, nil, modelKeyedResolver(byModel))

	r := gin.New()
	r.Use(middleware.RequestID())

	authed := r.Group("/api/v1")
	authed.Use(middleware.RequireAuth(systemPool, cfg))
	NewHandler(appPool, launcher).Register(authed)
	runsapi.NewHandler(appPool, engine).Register(authed)

	taskTokens := corea2a.NewTaskTokenSigner(cfg.A2ATaskTokenSecret)
	a2aHandler := a2aapi.NewHandler(appPool, launcher, taskTokens, "")
	authedA2A := r.Group("/api/v1")
	authedA2A.Use(middleware.RequireAuth(systemPool, cfg))
	a2aHandler.Register(authedA2A)
	ungatedA2A := r.Group("/api/v1")
	a2aHandler.RegisterTasksSend(ungatedA2A)

	server := httptest.NewServer(r)
	t.Cleanup(server.Close)

	launcher.SetA2AClient(corea2a.NewClient(server.URL, taskTokens))
	return server, launcher
}

type apiEnvelope struct {
	Data json.RawMessage `json:"data"`
}
type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func authedRequest(t *testing.T, cfg *config.Config, clerkUserID, method, url string, body any) *http.Request {
	t.Helper()
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
	req := httptest.NewRequest(method, url, reader)
	req.Header.Set("Content-Type", "application/json")
	token, err := devtoken.Sign(cfg.DevTokenSecret.Expose(), clerkUserID)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func TestTeamsHandler_CreateListGetDelete(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTeamTestFixture(t, systemPool)
	server, _ := buildTeamTestServer(t, appPool, systemPool, cfg, nil)

	createReq := createTeamRequest{
		Name: "Board Prep Team", OrchestratorAgentID: fx.orchestratorID.String(),
		Members: []createTeamMemberRequest{
			{AgentID: fx.financeID.String(), Role: "finance"},
			{AgentID: fx.opsID.String(), Role: "ops"},
		},
	}
	w := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(w, authedRequest(t, cfg, fx.userClerkID, http.MethodPost, "/api/v1/teams", createReq))
	if w.Code != http.StatusCreated {
		t.Fatalf("Create status = %d, body = %s", w.Code, w.Body.String())
	}
	var created teamSummary
	mustUnmarshalData(t, w.Body.Bytes(), &created)
	if created.ID == "" {
		t.Fatal("Create returned no team id")
	}

	// Duplicate role on the same team is rejected before anything is written.
	dupReq := createTeamRequest{
		Name: "Dup Role Team", OrchestratorAgentID: fx.orchestratorID.String(),
		Members: []createTeamMemberRequest{
			{AgentID: fx.financeID.String(), Role: "finance"},
			{AgentID: fx.opsID.String(), Role: "finance"},
		},
	}
	w = httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(w, authedRequest(t, cfg, fx.userClerkID, http.MethodPost, "/api/v1/teams", dupReq))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("Create with duplicate role status = %d, want 400", w.Code)
	}

	// List includes the created team with the right member count.
	w = httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(w, authedRequest(t, cfg, fx.userClerkID, http.MethodGet, "/api/v1/teams", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("List status = %d, body = %s", w.Code, w.Body.String())
	}
	var listed []teamSummary
	mustUnmarshalData(t, w.Body.Bytes(), &listed)
	found := false
	for _, tm := range listed {
		if tm.ID == created.ID {
			found = true
			if tm.MemberCount == nil || *tm.MemberCount != 2 {
				t.Fatalf("List member_count = %v, want 2", tm.MemberCount)
			}
		}
	}
	if !found {
		t.Fatal("List did not include the created team")
	}

	// Get returns both members with their roles.
	w = httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(w, authedRequest(t, cfg, fx.userClerkID, http.MethodGet, "/api/v1/teams/"+created.ID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("Get status = %d, body = %s", w.Code, w.Body.String())
	}
	var detail teamDetail
	mustUnmarshalData(t, w.Body.Bytes(), &detail)
	if len(detail.Members) != 2 {
		t.Fatalf("Get members = %d, want 2", len(detail.Members))
	}
	roles := map[string]bool{}
	for _, m := range detail.Members {
		roles[m.Role] = true
	}
	if !roles["finance"] || !roles["ops"] {
		t.Fatalf("Get members roles = %v, want finance and ops", roles)
	}

	// Delete soft-deletes it — a subsequent Get 404s.
	w = httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(w, authedRequest(t, cfg, fx.userClerkID, http.MethodDelete, "/api/v1/teams/"+created.ID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("Delete status = %d, body = %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(w, authedRequest(t, cfg, fx.userClerkID, http.MethodGet, "/api/v1/teams/"+created.ID, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("Get after Delete status = %d, want 404", w.Code)
	}
}

func TestTeamsHandler_CreateRejectsUnknownAgent(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTeamTestFixture(t, systemPool)
	server, _ := buildTeamTestServer(t, appPool, systemPool, cfg, nil)

	req := createTeamRequest{
		Name: "Bad Team", OrchestratorAgentID: fx.orchestratorID.String(),
		Members: []createTeamMemberRequest{{AgentID: "00000000-0000-0000-0000-000000000000", Role: "finance"}},
	}
	w := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(w, authedRequest(t, cfg, fx.userClerkID, http.MethodPost, "/api/v1/teams", req))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", w.Code, w.Body.String())
	}
	var apiErr apiError
	if err := json.Unmarshal(w.Body.Bytes(), &apiErr); err != nil {
		t.Fatal(err)
	}
	if apiErr.Error.Code != "AGENT_NOT_FOUND" {
		t.Fatalf("error code = %q, want AGENT_NOT_FOUND", apiErr.Error.Code)
	}
}

// TestTeamsHandler_RunEndToEnd is workflow 18's flagship test: a real
// orchestrator run that decomposes a task, dispatches 2 specialists in
// parallel over a genuine HTTP round trip (this test's own
// httptest.Server, not an in-process shortcut — see
// buildTeamTestServer's doc comment), and synthesizes their results —
// then verifies the full parent/child workflow_runs shape workflow 18
// added (parent_run_id, agent_id, delegated_role) and that
// GET /teams/{id}/runs/{run_id} surfaces both specialists.
func TestTeamsHandler_RunEndToEnd(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTeamTestFixture(t, systemPool)

	byModel := map[string]llm.ChatClient{}
	server, _ := buildTeamTestServer(t, appPool, systemPool, cfg, byModel)

	// Look up each agent's actual `model` column (set with a random
	// suffix by newTeamTestFixture) so byModel's keys line up exactly with
	// what buildRunDeps resolves against.
	var orchestratorModelCol, financeModelCol, opsModelCol string
	mustScanModel := func(id pgtype.UUID) string {
		var m string
		if err := systemPool.QueryRow(context.Background(), "select model from agents where id = $1", id).Scan(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	orchestratorModelCol = mustScanModel(fx.orchestratorID)
	financeModelCol = mustScanModel(fx.financeID)
	opsModelCol = mustScanModel(fx.opsID)

	byModel[orchestratorModelCol] = llm.NewMockChatClient(
		llm.ChatResponse{
			Content:    `{"subtasks":[{"role":"finance","task":"Summarize burn rate"},{"role":"ops","task":"Summarize hiring plan"}]}`,
			StopReason: llm.StopReasonEndTurn,
			Usage:      llm.TokenUsage{InputTokens: 20, OutputTokens: 10},
		},
		llm.ChatResponse{
			Content:    "Board summary: burn rate is stable and hiring is on track.",
			StopReason: llm.StopReasonEndTurn,
			Usage:      llm.TokenUsage{InputTokens: 30, OutputTokens: 15},
		},
	)
	byModel[financeModelCol] = llm.NewMockChatClient(llm.ChatResponse{
		Content: "Burn rate is $80k/month, 18 months of runway.", StopReason: llm.StopReasonEndTurn,
		Usage: llm.TokenUsage{InputTokens: 5, OutputTokens: 5},
	})
	byModel[opsModelCol] = llm.NewMockChatClient(llm.ChatResponse{
		Content: "3 open reqs, all on track to close this quarter.", StopReason: llm.StopReasonEndTurn,
		Usage: llm.TokenUsage{InputTokens: 5, OutputTokens: 5},
	})

	// Create the team via the real HTTP endpoint.
	createReq := createTeamRequest{
		Name: "Board Prep Team", OrchestratorAgentID: fx.orchestratorID.String(),
		Members: []createTeamMemberRequest{
			{AgentID: fx.financeID.String(), Role: "finance"},
			{AgentID: fx.opsID.String(), Role: "ops"},
		},
	}
	w := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(w, authedRequest(t, cfg, fx.userClerkID, http.MethodPost, "/api/v1/teams", createReq))
	if w.Code != http.StatusCreated {
		t.Fatalf("Create status = %d, body = %s", w.Code, w.Body.String())
	}
	var team teamSummary
	mustUnmarshalData(t, w.Body.Bytes(), &team)

	// Trigger the run.
	w = httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(w, authedRequest(t, cfg, fx.userClerkID, http.MethodPost, "/api/v1/teams/"+team.ID+"/run", runTeamRequest{Input: "Prepare Q2 board meeting"}))
	if w.Code != http.StatusAccepted {
		t.Fatalf("Run status = %d, body = %s", w.Code, w.Body.String())
	}
	var queued runQueuedResponse
	mustUnmarshalData(t, w.Body.Bytes(), &queued)

	// LaunchTeam is async — poll the parent run to completion.
	var parentStatus string
	var parentOutput *string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := systemPool.QueryRow(context.Background(),
			"select status, output from workflow_runs where id = $1", queued.RunID,
		).Scan(&parentStatus, &parentOutput); err != nil {
			t.Fatalf("poll parent run: %v", err)
		}
		if parentStatus == "completed" || parentStatus == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if parentStatus != "completed" {
		t.Fatalf("parent run status = %q, want completed (output=%v)", parentStatus, parentOutput)
	}
	if parentOutput == nil || *parentOutput != "Board summary: burn rate is stable and hiring is on track." {
		t.Fatalf("parent output = %v, want the synthesized summary", parentOutput)
	}

	// Fetch the aggregated trace and confirm both specialists ran under
	// the parent, with the right delegated_role/agent_id/status/output —
	// workflow 18's own acceptance criterion (each specialist's steps
	// visible in the trace) at the data layer this endpoint serves.
	w = httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(w, authedRequest(t, cfg, fx.userClerkID, http.MethodGet, "/api/v1/teams/"+team.ID+"/runs/"+queued.RunID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("Trace status = %d, body = %s", w.Code, w.Body.String())
	}
	var trace teamRunTrace
	mustUnmarshalData(t, w.Body.Bytes(), &trace)
	if len(trace.Specialists) != 2 {
		t.Fatalf("Trace specialists = %d, want 2 (body=%s)", len(trace.Specialists), w.Body.String())
	}
	byRole := map[string]childRun{}
	for _, s := range trace.Specialists {
		if s.Role == nil {
			t.Fatalf("specialist %s has no delegated role", s.ID)
		}
		byRole[*s.Role] = s
	}
	finance, ok := byRole["finance"]
	if !ok || finance.Status != "completed" || finance.Output == nil || *finance.Output != "Burn rate is $80k/month, 18 months of runway." {
		t.Fatalf("finance specialist = %+v", finance)
	}
	if finance.AgentID != fx.financeID.String() {
		t.Fatalf("finance specialist agent_id = %s, want %s", finance.AgentID, fx.financeID.String())
	}
	ops, ok := byRole["ops"]
	if !ok || ops.Status != "completed" || ops.Output == nil || *ops.Output != "3 open reqs, all on track to close this quarter." {
		t.Fatalf("ops specialist = %+v", ops)
	}

	// hours_saved accrues once, on the parent only — workflow 18's
	// double-counting guard (FinalizeRunHoursSaved's parent_run_id CASE).
	var parentHoursSaved *float64
	if err := systemPool.QueryRow(context.Background(), "select hours_saved from workflow_runs where id = $1", queued.RunID).Scan(&parentHoursSaved); err != nil {
		t.Fatal(err)
	}
	if parentHoursSaved == nil || *parentHoursSaved <= 0 {
		t.Fatalf("parent hours_saved = %v, want a positive value", parentHoursSaved)
	}
	var childHoursSaved sql.NullFloat64
	if err := systemPool.QueryRow(context.Background(), "select hours_saved from workflow_runs where id = $1", finance.ID).Scan(&childHoursSaved); err != nil {
		t.Fatal(err)
	}
	if childHoursSaved.Valid {
		t.Fatalf("specialist hours_saved = %v, want NULL (not double-counted)", childHoursSaved.Float64)
	}

	// GET /teams/{id}/runs — the "Recent runs" list a founder needs to get
	// back to a past run after navigating away, added specifically because
	// there was previously no way to do that at all (see ListRuns's own
	// doc comment). Only the parent (orchestrator) run should appear here,
	// same parent_run_id IS NULL scoping the flat GET /runs list uses.
	w = httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(w, authedRequest(t, cfg, fx.userClerkID, http.MethodGet, "/api/v1/teams/"+team.ID+"/runs", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("ListRuns status = %d, body = %s", w.Code, w.Body.String())
	}
	var teamRuns []teamRunSummary
	mustUnmarshalData(t, w.Body.Bytes(), &teamRuns)
	if len(teamRuns) != 1 {
		t.Fatalf("ListRuns returned %d runs, want 1 (specialists must not appear here)", len(teamRuns))
	}
	if teamRuns[0].ID != queued.RunID || teamRuns[0].Status != "completed" {
		t.Fatalf("ListRuns[0] = %+v, want id=%s status=completed", teamRuns[0], queued.RunID)
	}

	// GET /runs (the flat, org-wide list) must also carry team_id for this
	// same run, so the frontend can badge/link it correctly instead of
	// rendering it like an ordinary single-agent row — the second half of
	// the same reported confusion ListRuns above fixes the first half of.
	w = httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(w, authedRequest(t, cfg, fx.userClerkID, http.MethodGet, "/api/v1/runs", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /runs status = %d, body = %s", w.Code, w.Body.String())
	}
	var runsEnv struct {
		Data struct {
			Runs []struct {
				ID     string  `json:"id"`
				TeamID *string `json:"team_id"`
			} `json:"runs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &runsEnv); err != nil {
		t.Fatalf("unmarshal GET /runs response: %v (body=%s)", err, w.Body.String())
	}
	found := false
	for _, r := range runsEnv.Data.Runs {
		if r.ID != queued.RunID {
			continue
		}
		found = true
		if r.TeamID == nil || *r.TeamID != team.ID {
			t.Fatalf("GET /runs team_id for the orchestrator's own run = %v, want %s", r.TeamID, team.ID)
		}
	}
	if !found {
		t.Fatalf("GET /runs did not include the team's orchestrator run %s", queued.RunID)
	}
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

func postTeam(t *testing.T, server *httptest.Server, cfg *config.Config, user string, req createTeamRequest) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(w, authedRequest(t, cfg, user, http.MethodPost, "/api/v1/teams", req))
	var env apiEnvelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(env.Data, &created)
	if w.Code == http.StatusCreated {
		return w.Code, created.ID
	}
	var e apiError
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	return w.Code, e.Error.Code
}

func TestTeamsHandler_CreateBoundsTheTeamSize(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTeamTestFixture(t, systemPool)
	server, _ := buildTeamTestServer(t, appPool, systemPool, cfg, nil)

	member := func(role string) createTeamMemberRequest {
		return createTeamMemberRequest{AgentID: fx.financeID.String(), Role: role}
	}
	many := make([]createTeamMemberRequest, 0, 40)
	for i := 0; i < 40; i++ {
		many = append(many, member(fmt.Sprintf("role-%d", i)))
	}
	long := func(n int) string { return strings.Repeat("a", n) }
	desc := long(2001)

	for name, req := range map[string]createTeamRequest{
		"too many members": {Name: "T", OrchestratorAgentID: fx.orchestratorID.String(), Members: many},
		"name too long":    {Name: long(256), OrchestratorAgentID: fx.orchestratorID.String(), Members: []createTeamMemberRequest{member("a")}},
		"role too long":    {Name: "T", OrchestratorAgentID: fx.orchestratorID.String(), Members: []createTeamMemberRequest{member(long(101))}},
		"description long": {Name: "T", Description: &desc, OrchestratorAgentID: fx.orchestratorID.String(), Members: []createTeamMemberRequest{member("a")}},
	} {
		if code, _ := postTeam(t, server, cfg, fx.userClerkID, req); code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, code)
		}
	}
	if code, _ := postTeam(t, server, cfg, fx.userClerkID, createTeamRequest{
		Name: "Fine", OrchestratorAgentID: fx.orchestratorID.String(), Members: []createTeamMemberRequest{member("a"), member("b")},
	}); code != http.StatusCreated {
		t.Fatalf("a normal team = %d, want 201", code)
	}
}

func TestTeamsHandler_RunGuards(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTeamTestFixture(t, systemPool)
	server, _ := buildTeamTestServer(t, appPool, systemPool, cfg, nil)

	code, teamID := postTeam(t, server, cfg, fx.userClerkID, createTeamRequest{
		Name: "Guarded", OrchestratorAgentID: fx.orchestratorID.String(),
		Members: []createTeamMemberRequest{{AgentID: fx.financeID.String(), Role: "finance"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create team = %d", code)
	}
	run := func(input string) (int, string) {
		w := httptest.NewRecorder()
		server.Config.Handler.ServeHTTP(w, authedRequest(t, cfg, fx.userClerkID, http.MethodPost, "/api/v1/teams/"+teamID+"/run", map[string]string{"input": input}))
		var e apiError
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		return w.Code, e.Error.Code
	}

	t.Run("an over-long input is refused", func(t *testing.T) {
		if code, errCode := run(strings.Repeat("x", 20001)); code != http.StatusBadRequest || errCode != "INPUT_TOO_LONG" {
			t.Fatalf("got (%d, %s), want (400, INPUT_TOO_LONG)", code, errCode)
		}
	})

	t.Run("a deleted specialist or orchestrator blocks the run", func(t *testing.T) {
		for _, agent := range []pgtype.UUID{fx.financeID, fx.orchestratorID} {
			if _, err := systemPool.Exec(context.Background(), "update agents set is_active = false where id = $1", agent); err != nil {
				t.Fatal(err)
			}
			if code, errCode := run("do the thing"); code != http.StatusConflict || errCode != "AGENT_INACTIVE" {
				t.Fatalf("got (%d, %s), want (409, AGENT_INACTIVE)", code, errCode)
			}
			if _, err := systemPool.Exec(context.Background(), "update agents set is_active = true where id = $1", agent); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("the workspace's concurrent-run cap applies to team runs too", func(t *testing.T) {
		var workflowID pgtype.UUID
		if err := systemPool.QueryRow(context.Background(), "select id from workflows where team_id = $1", teamID).Scan(&workflowID); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < graph.MaxActiveRunsPerOrg; i++ {
			if _, err := systemPool.Exec(context.Background(),
				"insert into workflow_runs (workflow_id, org_id, status) values ($1, $2, 'running')", workflowID, fx.orgPg); err != nil {
				t.Fatal(err)
			}
		}
		if code, errCode := run("one more"); code != http.StatusTooManyRequests || errCode != "TOO_MANY_ACTIVE_RUNS" {
			t.Fatalf("got (%d, %s), want (429, TOO_MANY_ACTIVE_RUNS)", code, errCode)
		}
	})
}

func TestTeamsHandler_TraceOnlyServesTheTeamsOwnRuns(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTeamTestFixture(t, systemPool)
	server, _ := buildTeamTestServer(t, appPool, systemPool, cfg, nil)

	mk := func(name string) string {
		_, id := postTeam(t, server, cfg, fx.userClerkID, createTeamRequest{
			Name: name, OrchestratorAgentID: fx.orchestratorID.String(),
			Members: []createTeamMemberRequest{{AgentID: fx.financeID.String(), Role: "finance"}},
		})
		return id
	}
	teamA, teamB := mk("A"), mk("B")
	var runID string
	if err := systemPool.QueryRow(context.Background(),
		`insert into workflow_runs (workflow_id, org_id, status) select id, org_id, 'completed' from workflows where team_id = $1 returning id::text`, teamA).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	get := func(team string) int {
		w := httptest.NewRecorder()
		server.Config.Handler.ServeHTTP(w, authedRequest(t, cfg, fx.userClerkID, http.MethodGet, "/api/v1/teams/"+team+"/runs/"+runID, nil))
		return w.Code
	}
	if c := get(teamA); c != http.StatusOK {
		t.Fatalf("its own team's trace = %d, want 200", c)
	}
	if c := get(teamB); c != http.StatusNotFound {
		t.Fatalf("another team's id for the same run = %d, want 404", c)
	}
}

func TestTeamsHandler_TeamCapPerWorkspace(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTeamTestFixture(t, systemPool)
	server, _ := buildTeamTestServer(t, appPool, systemPool, cfg, nil)

	if _, err := systemPool.Exec(context.Background(), fmt.Sprintf(`
		insert into agent_teams (org_id, name, orchestrator_agent_id)
		select $1, 'seed team '||g, $2 from generate_series(1, %d) g`, maxTeamsPerOrg), fx.orgPg, fx.orchestratorID); err != nil {
		t.Fatalf("seed teams: %v", err)
	}
	code, errCode := postTeam(t, server, cfg, fx.userClerkID, createTeamRequest{
		Name: "One too many", OrchestratorAgentID: fx.orchestratorID.String(),
		Members: []createTeamMemberRequest{{AgentID: fx.financeID.String(), Role: "finance"}},
	})
	if code != http.StatusBadRequest || errCode != "TEAM_LIMIT_REACHED" {
		t.Fatalf("got (%d, %s), want (400, TEAM_LIMIT_REACHED)", code, errCode)
	}
}

// Without the A2A client (no A2A_TASK_TOKEN_SECRET) a team run can't work:
// the API must say so up front, not insert a run and then fail.
func TestTeamsHandler_RunIsRefusedWhenTeamRunsAreNotConfigured(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTeamTestFixture(t, systemPool)

	gin.SetMode(gin.TestMode)
	registry, err := coremcp.NewRegistry(context.Background(), map[string]*gomcp.Server{})
	if err != nil {
		t.Fatal(err)
	}
	launcher := graph.NewLauncherWithResolver(graph.NewEngine(appPool), appPool, nil, registry, nil, nil, modelKeyedResolver(nil))
	if launcher.TeamRunsAvailable() {
		t.Fatal("a launcher with no A2A client claims team runs are available")
	}
	r := gin.New()
	r.Use(middleware.RequestID())
	authed := r.Group("/api/v1")
	authed.Use(middleware.RequireAuth(systemPool, cfg))
	NewHandler(appPool, launcher).Register(authed)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(t, cfg, fx.userClerkID, http.MethodPost, "/api/v1/teams/"+uuid.NewString()+"/run", map[string]string{"input": "go"}))
	var e apiError
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != http.StatusServiceUnavailable || e.Error.Code != "TEAM_RUNS_NOT_CONFIGURED" {
		t.Fatalf("got (%d, %s), want (503, TEAM_RUNS_NOT_CONFIGURED)", w.Code, e.Error.Code)
	}
	var runs int
	if err := systemPool.QueryRow(context.Background(), "select count(*) from workflow_runs where org_id = $1", fx.orgPg).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("%d run rows were inserted despite the refusal", runs)
	}
}

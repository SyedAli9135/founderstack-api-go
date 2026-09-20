//go:build integration

package a2a

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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/config"
	corea2a "github.com/founderstack/api/internal/core/a2a"
	"github.com/founderstack/api/internal/core/graph"
	coremcp "github.com/founderstack/api/internal/core/mcp"
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

// a2aTestFixture builds a minimal org with a real team (one orchestrator +
// one "finance" specialist) plus a lone, team-less agent — enough to
// exercise both Manifest's IsAgentOnActiveTeam gate and TasksSend's
// GetRunTeamAndWorkflow/GetAgentTeamMemberRole authorization chain
// without needing a real run to actually complete (unlike
// internal/api/teams's own end-to-end test, this file is about the auth
// boundary, not the happy path it already covers).
type a2aTestFixture struct {
	orgPg                                     pgtype.UUID
	clerkUserID                               string
	teamID, orchestratorID, financeID, loneID pgtype.UUID
	dispatchingRunID                          uuid.UUID
}

func newA2ATestFixture(t *testing.T, systemPool *pgxpool.Pool) a2aTestFixture {
	t.Helper()
	ctx := context.Background()
	suffix := randSuffix(t)
	fx := a2aTestFixture{clerkUserID: "user_a2a_test_" + suffix}

	if err := systemPool.QueryRow(ctx,
		"insert into organizations (clerk_org_id, name, slug) values ($1, 'A2A Test Org', $2) returning id",
		"org_a2a_test_"+suffix, "a2a-test-"+suffix,
	).Scan(&fx.orgPg); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", fx.orgPg)
	})

	if _, err := systemPool.Exec(ctx,
		`insert into users (org_id, clerk_user_id, email, role) values ($1, $2, 'a2a-test@example.com', 'owner')`,
		fx.orgPg, fx.clerkUserID,
	); err != nil {
		t.Fatalf("insert test user: %v", err)
	}

	insertAgent := func(name, slug string) pgtype.UUID {
		var id pgtype.UUID
		if err := systemPool.QueryRow(ctx,
			`insert into agents (org_id, name, slug, system_prompt, model, policy_scope) values ($1, $2, $3, 'You are a test agent.', 'test-model', '{"allowed_tools":["fake.get_data"]}'::jsonb) returning id`,
			fx.orgPg, name, slug+"-"+suffix,
		).Scan(&id); err != nil {
			t.Fatalf("insert test agent %s: %v", name, err)
		}
		return id
	}
	fx.orchestratorID = insertAgent("Orchestrator", "orchestrator")
	fx.financeID = insertAgent("Finance Agent", "finance-agent")
	fx.loneID = insertAgent("Lone Agent", "lone-agent") // never joins a team

	if err := systemPool.QueryRow(ctx,
		`insert into agent_teams (org_id, name, orchestrator_agent_id) values ($1, 'Test Team', $2) returning id`,
		fx.orgPg, fx.orchestratorID,
	).Scan(&fx.teamID); err != nil {
		t.Fatalf("insert test team: %v", err)
	}
	if _, err := systemPool.Exec(ctx,
		`insert into agent_team_members (team_id, agent_id, role) values ($1, $2, 'finance')`,
		fx.teamID, fx.financeID,
	); err != nil {
		t.Fatalf("insert test team member: %v", err)
	}

	var workflowID pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into workflows (org_id, agent_id, team_id, name, trigger_type, graph_definition) values ($1, $2, $3, 'Test Team Workflow', 'manual', '{"type":"team"}'::jsonb) returning id`,
		fx.orgPg, fx.orchestratorID, fx.teamID,
	).Scan(&workflowID); err != nil {
		t.Fatalf("insert test team workflow: %v", err)
	}

	var runPg pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into workflow_runs (workflow_id, org_id, agent_id, status) values ($1, $2, $3, 'running') returning id`,
		workflowID, fx.orgPg, fx.orchestratorID,
	).Scan(&runPg); err != nil {
		t.Fatalf("insert test dispatching run: %v", err)
	}
	fx.dispatchingRunID = uuid.UUID(runPg.Bytes)

	return fx
}

func a2aTestConfig() *config.Config {
	return &config.Config{AppEnv: "development", DevTokenSecret: "test-dev-token-secret"}
}

func a2aTestServer(t *testing.T, appPool, systemPool *pgxpool.Pool, cfg *config.Config, tokens *corea2a.TaskTokenSigner) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)

	engine := graph.NewEngine(appPool)
	registry, err := coremcp.NewRegistry(context.Background(), map[string]*gomcp.Server{})
	if err != nil {
		t.Fatalf("build empty mcp registry: %v", err)
	}
	launcher := graph.NewLauncher(engine, appPool, nil, registry, nil, nil)

	r := gin.New()
	handler := NewHandler(appPool, launcher, tokens, "")

	authed := r.Group("/api/v1")
	authed.Use(middleware.RequireAuth(systemPool, cfg))
	handler.Register(authed)

	ungated := r.Group("/api/v1")
	handler.RegisterTasksSend(ungated)

	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	return server
}

func devAuthHeader(t *testing.T, cfg *config.Config, clerkUserID string) string {
	t.Helper()
	token, err := devtoken.Sign(cfg.DevTokenSecret.Expose(), clerkUserID)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + token
}

func TestHandler_Manifest_NotOnAnyTeamReturns404(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := a2aTestConfig()
	fx := newA2ATestFixture(t, systemPool)
	tokens := corea2a.NewTaskTokenSigner("test-secret")
	server := a2aTestServer(t, appPool, systemPool, cfg, tokens)

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/a2a/agents/"+fx.loneID.String()+"/.well-known/agent.json", nil)
	req.Header.Set("Authorization", devAuthHeader(t, cfg, fx.clerkUserID))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an agent that belongs to no team", resp.StatusCode)
	}
}

func TestHandler_Manifest_TeamMemberReturnsCard(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := a2aTestConfig()
	fx := newA2ATestFixture(t, systemPool)
	tokens := corea2a.NewTaskTokenSigner("test-secret")
	server := a2aTestServer(t, appPool, systemPool, cfg, tokens)

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/a2a/agents/"+fx.financeID.String()+"/.well-known/agent.json", nil)
	req.Header.Set("Authorization", devAuthHeader(t, cfg, fx.clerkUserID))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var card corea2a.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatal(err)
	}
	if card.Name != "Finance Agent" {
		t.Fatalf("card.Name = %q, want %q", card.Name, "Finance Agent")
	}
	if len(card.Skills) != 1 || card.Skills[0].ID != "fake.get_data" {
		t.Fatalf("card.Skills = %+v, want one skill for fake.get_data", card.Skills)
	}
}

func TestHandler_TasksSend_MissingTokenRejected(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := a2aTestConfig()
	fx := newA2ATestFixture(t, systemPool)
	tokens := corea2a.NewTaskTokenSigner("test-secret")
	server := a2aTestServer(t, appPool, systemPool, cfg, tokens)

	body, _ := json.Marshal(corea2a.TaskSendRequest{
		JSONRPC: "2.0", ID: "req-1", Method: "tasks/send",
		Params: corea2a.TaskSendParams{ID: uuid.NewString(), SessionID: fx.dispatchingRunID.String(), Message: corea2a.TextMessage("user", "do something")},
	})
	resp, err := http.Post(server.URL+"/api/v1/a2a/agents/"+fx.financeID.String()+"/tasks/send", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 with no Authorization header", resp.StatusCode)
	}
}

func TestHandler_TasksSend_WrongAgentTokenRejected(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := a2aTestConfig()
	fx := newA2ATestFixture(t, systemPool)
	tokens := corea2a.NewTaskTokenSigner("test-secret")
	server := a2aTestServer(t, appPool, systemPool, cfg, tokens)

	// Token minted for the lone agent, presented against the finance
	// agent's endpoint — TaskTokenSigner.Verify must reject this even
	// though the org id matches.
	wrongToken := tokens.Sign(uuid.UUID(fx.orgPg.Bytes), uuid.UUID(fx.loneID.Bytes), time.Now().Add(time.Hour))

	body, _ := json.Marshal(corea2a.TaskSendRequest{
		JSONRPC: "2.0", ID: "req-1", Method: "tasks/send",
		Params: corea2a.TaskSendParams{ID: uuid.NewString(), SessionID: fx.dispatchingRunID.String(), Message: corea2a.TextMessage("user", "do something")},
	})
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/a2a/agents/"+fx.financeID.String()+"/tasks/send", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+wrongToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a token minted for a different agent", resp.StatusCode)
	}
}

// TestHandler_TasksSend_NonMemberAgentForbidden is the real authorization
// check this endpoint exists for: a validly-signed token for the *lone*
// agent (which the signer alone can't distinguish from a legitimate team
// member — Verify only proves "this org, this exact agent") is still
// rejected because that agent isn't actually a member of the dispatching
// run's team.
func TestHandler_TasksSend_NonMemberAgentForbidden(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := a2aTestConfig()
	fx := newA2ATestFixture(t, systemPool)
	tokens := corea2a.NewTaskTokenSigner("test-secret")
	server := a2aTestServer(t, appPool, systemPool, cfg, tokens)

	validToken := tokens.Sign(uuid.UUID(fx.orgPg.Bytes), uuid.UUID(fx.loneID.Bytes), time.Now().Add(time.Hour))
	body, _ := json.Marshal(corea2a.TaskSendRequest{
		JSONRPC: "2.0", ID: "req-1", Method: "tasks/send",
		Params: corea2a.TaskSendParams{ID: uuid.NewString(), SessionID: fx.dispatchingRunID.String(), Message: corea2a.TextMessage("user", "do something")},
	})
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/a2a/agents/"+fx.loneID.String()+"/tasks/send", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+validToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for an agent not on the dispatching run's team", resp.StatusCode)
	}
}

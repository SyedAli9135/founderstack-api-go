//go:build integration

package agents

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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/config"
	coremcp "github.com/founderstack/api/internal/core/mcp"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
	"github.com/founderstack/api/internal/pkg/devtoken"
)

// fakeToolRegistry builds a real coremcp.Registry wired to 2 tiny
// in-memory MCP servers (stripe: get_mrr, slack: send_message) — not real
// Stripe/Slack, same "don't depend on a live third-party service to keep
// the suite green" reasoning as internal/core/mcp/gateway_integration_test.go's
// echoTokenServer. ListTools only introspects schemas; nothing here ever
// calls a tool.
func fakeToolRegistry(t *testing.T) *coremcp.Registry {
	t.Helper()
	stripe := gomcp.NewServer(&gomcp.Implementation{Name: "stripe", Version: "1.0.0"}, nil)
	gomcp.AddTool(stripe, &gomcp.Tool{Name: "get_mrr", Description: "Estimate MRR"},
		func(ctx context.Context, req *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})

	slack := gomcp.NewServer(&gomcp.Implementation{Name: "slack", Version: "1.0.0"}, nil)
	gomcp.AddTool(slack, &gomcp.Tool{Name: "send_message", Description: "Send a Slack message"},
		func(ctx context.Context, req *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})

	registry, err := coremcp.NewRegistry(context.Background(), map[string]*gomcp.Server{
		"stripe": stripe,
		"slack":  slack,
	})
	if err != nil {
		t.Fatalf("build fake tool registry: %v", err)
	}
	return registry
}

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

func testOrgAndUser(t *testing.T, systemPool *pgxpool.Pool) (orgID pgtype.UUID, clerkUserID string) {
	t.Helper()
	suffix := randSuffix(t)
	clerkUserID = "user_agents_test_" + suffix
	ctx := context.Background()

	err := systemPool.QueryRow(ctx,
		"insert into organizations (clerk_org_id, name, slug) values ($1, 'Agents Test Org', $2) returning id",
		"org_agents_test_"+suffix, "agents-test-"+suffix,
	).Scan(&orgID)
	if err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	_, err = systemPool.Exec(ctx,
		`insert into users (org_id, clerk_user_id, email) values ($1, $2, 'agents-test@example.com')`,
		orgID, clerkUserID,
	)
	if err != nil {
		t.Fatalf("insert test user: %v", err)
	}

	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID)
	})
	return orgID, clerkUserID
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{AppEnv: "development", DevTokenSecret: "test-dev-token-secret"}
}

func testRouter(t *testing.T, systemPool, appPool *pgxpool.Pool, cfg *config.Config, registry *coremcp.Registry) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())

	h := NewHandler(appPool, registry)
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

func connectService(t *testing.T, appPool *pgxpool.Pool, orgID pgtype.UUID, service string) {
	t.Helper()
	active := "connected"
	err := tenant.WithTx(context.Background(), appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		_, err := q.UpsertConnection(ctx, dbgen.UpsertConnectionParams{
			OrgID: orgID, ServiceName: service, OauthStatus: &active,
		})
		return err
	})
	if err != nil {
		t.Fatalf("connect %s: %v", service, err)
	}
}

const validSystemPrompt = "You are a finance agent that helps founders understand their Stripe revenue metrics and subscriptions."

func TestAgentsHandler_FullLifecycle(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	orgID, clerkUserID := testOrgAndUser(t, systemPool)
	registry := fakeToolRegistry(t)
	router := testRouter(t, systemPool, appPool, cfg, registry)

	connectService(t, appPool, orgID, "stripe")
	connectService(t, appPool, orgID, "slack")

	t.Run("unauthenticated list is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("available tools reflects only connected services", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/agents/tools", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		var env apiEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		var tools []toolOption
		if err := json.Unmarshal(env.Data, &tools); err != nil {
			t.Fatal(err)
		}
		if len(tools) != 2 {
			t.Fatalf("got %d tools, want 2 (stripe.get_mrr + slack.send_message); body = %s", len(tools), rec.Body.String())
		}
	})

	t.Run("create rejects a missing system_prompt", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/agents", map[string]any{"name": "X"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("create rejects a system_prompt under 50 chars", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/agents", map[string]any{
			"name": "X", "system_prompt": "too short",
			"policy_scope": map[string]any{"allowed_tools": []string{"stripe.get_mrr"}},
		})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("create rejects zero allowed tools", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/agents", map[string]any{
			"name": "X", "system_prompt": validSystemPrompt,
			"policy_scope": map[string]any{"allowed_tools": []string{}},
		})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("create rejects an unknown tool", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/agents", map[string]any{
			"name": "X", "system_prompt": validSystemPrompt,
			"policy_scope": map[string]any{"allowed_tools": []string{"stripe.nonexistent"}},
		})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("create rejects a non-positive cost cap", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/agents", map[string]any{
			"name": "X", "system_prompt": validSystemPrompt,
			"policy_scope": map[string]any{"allowed_tools": []string{"stripe.get_mrr"}, "max_cost_per_run_usd": -1},
		})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	var agentID string
	t.Run("create succeeds with valid input and applies defaults", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "Finance Agent",
			"system_prompt": validSystemPrompt,
			"policy_scope": map[string]any{
				"allowed_tools": []string{"stripe.get_mrr"},
			},
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
		var got agentView
		if err := json.Unmarshal(env.Data, &got); err != nil {
			t.Fatal(err)
		}
		if got.ID == "" || got.Slug != "finance-agent" || got.AgentType != defaultAgentType ||
			got.Model != defaultModel || got.MaxOutputTokens != defaultMaxOutputTokens || !got.IsActive {
			t.Fatalf("unexpected created agent: %+v", got)
		}
		agentID = got.ID
	})

	t.Run("duplicate name is rejected", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/agents", map[string]any{
			"name": "Finance Agent", "system_prompt": validSystemPrompt,
			"policy_scope": map[string]any{"allowed_tools": []string{"stripe.get_mrr"}},
		})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("get returns the created agent", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/agents/"+agentID, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("get unknown id is 404", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/agents/00000000-0000-0000-0000-000000000000", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("get invalid id is 400", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/agents/not-a-uuid", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("list includes the created agent", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/agents", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var env apiEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		var list []agentView
		if err := json.Unmarshal(env.Data, &list); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, a := range list {
			if a.ID == agentID {
				found = true
			}
		}
		if !found {
			t.Fatalf("created agent %s not in list", agentID)
		}
	})

	t.Run("update rejects a too-short system_prompt", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPatch, "/api/v1/agents/"+agentID, map[string]any{"system_prompt": "short"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("partial update only changes the given fields", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPatch, "/api/v1/agents/"+agentID, map[string]any{
			"description": "updated", "temperature": 0.9,
		})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		var env apiEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		var got agentView
		if err := json.Unmarshal(env.Data, &got); err != nil {
			t.Fatal(err)
		}
		if got.Description == nil || *got.Description != "updated" || got.Temperature != 0.9 {
			t.Fatalf("update didn't apply: %+v", got)
		}
		if got.Name != "Finance Agent" || got.SystemPrompt != validSystemPrompt {
			t.Fatalf("untouched fields changed: %+v", got)
		}
		if got.Version != 2 {
			t.Fatalf("version = %d, want 2", got.Version)
		}
	})

	t.Run("update on unknown id is 404", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPatch, "/api/v1/agents/00000000-0000-0000-0000-000000000000", map[string]any{"description": "x"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("plan limit blocks a 4th active agent", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/agents", map[string]any{
				"name": "Extra Agent " + string(rune('A'+i)), "system_prompt": validSystemPrompt,
				"policy_scope": map[string]any{"allowed_tools": []string{"slack.send_message"}},
			})
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("setup agent %d: status = %d, want 201; body = %s", i, rec.Code, rec.Body.String())
			}
		}

		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/agents", map[string]any{
			"name": "One Too Many", "system_prompt": validSystemPrompt,
			"policy_scope": map[string]any{"allowed_tools": []string{"slack.send_message"}},
		})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (default max_agents=3 already reached); body = %s", rec.Code, rec.Body.String())
		}
		var env apiEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if env.Error.Code != "PLAN_LIMIT_REACHED" {
			t.Fatalf("error code = %q, want PLAN_LIMIT_REACHED", env.Error.Code)
		}
	})

	t.Run("delete removes the agent and frees its name and its plan-limit slot", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodDelete, "/api/v1/agents/"+agentID, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}

		// GetAgent deliberately doesn't filter by is_active — a soft-deleted
		// agent's config still exists and stays fetchable by id ("without
		// losing run history" means a later run-history view needs to be
		// able to show what config a historical run used, even after the
		// agent itself was deactivated). Only List (and the plan-limit
		// count) exclude inactive agents.
		getReq := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/agents/"+agentID, nil)
		getRec := httptest.NewRecorder()
		router.ServeHTTP(getRec, getReq)
		if getRec.Code != http.StatusOK {
			t.Fatalf("get after delete: status = %d, want 200; body = %s", getRec.Code, getRec.Body.String())
		}
		var env apiEnvelope
		if err := json.Unmarshal(getRec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		var got agentView
		if err := json.Unmarshal(env.Data, &got); err != nil {
			t.Fatal(err)
		}
		if got.IsActive {
			t.Fatal("agent still shows is_active=true after delete")
		}

		reuseReq := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/agents", map[string]any{
			"name": "Finance Agent", "system_prompt": validSystemPrompt,
			"policy_scope": map[string]any{"allowed_tools": []string{"stripe.get_mrr"}},
		})
		reuseRec := httptest.NewRecorder()
		router.ServeHTTP(reuseRec, reuseReq)
		if reuseRec.Code != http.StatusCreated {
			t.Fatalf("reusing a deleted agent's name: status = %d, want 201; body = %s", reuseRec.Code, reuseRec.Body.String())
		}
	})

	t.Run("delete on unknown id is 404", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodDelete, "/api/v1/agents/00000000-0000-0000-0000-000000000000", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

//go:build integration

package approvals

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/core/graph"
	"github.com/founderstack/api/internal/core/integrations"
	"github.com/founderstack/api/internal/core/llm"
	coremcp "github.com/founderstack/api/internal/core/mcp"
	"github.com/founderstack/api/internal/core/notify"
	"github.com/founderstack/api/internal/pkg/devtoken"
	"github.com/founderstack/api/internal/pkg/secret"
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

// fakeDestructiveToolServer registers one write_destructive_or_financial
// tool — enough to drive a real suspend, same shape as
// internal/core/graph/nodes_integration_test.go's fakeToolServer (can't
// import it directly, different package).
func fakeDestructiveToolServer() *gomcp.Server {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "fake", Version: "1.0.0"}, nil)
	gomcp.AddTool(server, &gomcp.Tool{
		Name: "delete_thing", Description: "Delete something — destructive.",
		Annotations: coremcp.DestructiveOrFinancial(),
	}, func(ctx context.Context, req *gomcp.CallToolRequest, in struct {
		ID string `json:"id,omitempty"`
	}) (*gomcp.CallToolResult, struct {
		Deleted bool `json:"deleted"`
	}, error) {
		return nil, struct {
			Deleted bool `json:"deleted"`
		}{Deleted: true}, nil
	})
	return server
}

// approvalFixture is the org/user(s)/agent/workflow chain a real suspended
// run needs — a superset of graph package's launchFixture, since this
// handler additionally needs users with/without can_approve_workflows.
type approvalFixture struct {
	orgID                  pgtype.UUID
	approverUserID         pgtype.UUID
	approverClerkID        string
	nonApproverClerkUserID string
	agentID, workflowID    pgtype.UUID
}

func newApprovalFixture(t *testing.T, systemPool, appPool *pgxpool.Pool) approvalFixture {
	t.Helper()
	ctx := context.Background()
	suffix := randSuffix(t)

	var orgID pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into organizations (clerk_org_id, name, slug, llm_provider) values ($1, 'Approvals Test Org', $2, 'anthropic') returning id`,
		"org_approvals_test_"+suffix, "approvals-test-"+suffix,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	t.Cleanup(func() { _, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID) })

	approverClerkID := "user_approver_" + suffix
	var approverUserID pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into users (org_id, clerk_user_id, email, can_approve_workflows) values ($1, $2, $3, true) returning id`,
		orgID, approverClerkID, "approver-"+suffix+"@example.com",
	).Scan(&approverUserID); err != nil {
		t.Fatalf("insert approver user: %v", err)
	}

	nonApproverClerkID := "user_nonapprover_" + suffix
	if _, err := systemPool.Exec(ctx,
		`insert into users (org_id, clerk_user_id, email, can_approve_workflows) values ($1, $2, $3, false)`,
		orgID, nonApproverClerkID, "nonapprover-"+suffix+"@example.com",
	); err != nil {
		t.Fatalf("insert non-approver user: %v", err)
	}

	policyJSON := []byte(`{"allowed_tools":["fake.delete_thing"]}`)
	var agentID pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into agents (org_id, name, slug, system_prompt, model, policy_scope) values ($1, 'Approvals Test Agent', $2, 'test', 'test-model', $3) returning id`,
		orgID, "approvals-test-agent-"+suffix, policyJSON,
	).Scan(&agentID); err != nil {
		t.Fatalf("insert test agent: %v", err)
	}

	var workflowID pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into workflows (org_id, agent_id, name, trigger_type, graph_definition) values ($1, $2, 'Approvals Test Workflow', 'manual', '{}'::jsonb) returning id`,
		orgID, agentID,
	).Scan(&workflowID); err != nil {
		t.Fatalf("insert test workflow: %v", err)
	}

	if _, err := systemPool.Exec(ctx,
		`insert into api_key_registry (org_id, provider, key_prefix, encrypted_key, kms_key_id, is_valid) values ($1, 'anthropic', 'sk-ant-...', 'not-a-real-ciphertext', 'local-aes-gcm', true)`,
		orgID,
	); err != nil {
		t.Fatalf("insert test key: %v", err)
	}

	if err := integrations.SaveConnection(ctx, appPool, make([]byte, 32), orgID, "fake", "Fake", "manual", "connected",
		integrations.Token{AccessToken: "fake-token"},
	); err != nil {
		t.Fatalf("save fake connection: %v", err)
	}

	return approvalFixture{
		orgID: orgID, approverUserID: approverUserID, nonApproverClerkUserID: nonApproverClerkID,
		approverClerkID: approverClerkID, agentID: agentID, workflowID: workflowID,
	}
}

// suspendNewRun launches a fresh run against fx's agent/workflow through a
// real graph.Launcher (fake destructive tool, MockChatClient) and polls
// until it's actually awaiting_approval with a real approvals row —
// exactly the state a human would find it in. Returns the run id and the
// approvals row's id.
func suspendNewRun(t *testing.T, systemPool, appPool *pgxpool.Pool, fx approvalFixture, launcher *graph.Launcher) (runID, approvalID pgtype.UUID) {
	t.Helper()
	ctx := context.Background()

	if err := systemPool.QueryRow(ctx,
		`insert into workflow_runs (workflow_id, org_id, status) values ($1, $2, 'pending') returning id`,
		fx.workflowID, fx.orgID,
	).Scan(&runID); err != nil {
		t.Fatalf("insert test workflow_run: %v", err)
	}

	launcher.Launch(uuidFromPg(fx.orgID), uuidFromPg(fx.agentID), uuidFromPg(fx.workflowID), uuidFromPg(runID), "delete abc")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := systemPool.QueryRow(ctx, "select status from workflow_runs where id = $1", runID).Scan(&status); err != nil {
			t.Fatalf("poll run status: %v", err)
		}
		if status == "awaiting_approval" {
			if err := systemPool.QueryRow(ctx, "select id from approvals where run_id = $1", runID).Scan(&approvalID); err != nil {
				t.Fatalf("fetch approvals row: %v", err)
			}
			return runID, approvalID
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run never reached awaiting_approval within the test deadline")
	return
}

func testRouter(t *testing.T, systemPool, appPool *pgxpool.Pool, cfg *config.Config, launcher *graph.Launcher, tokens *notify.ActionTokenSigner) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())

	h := NewHandler(appPool, systemPool, launcher, tokens, cfg)
	authed := r.Group("/api/v1")
	authed.Use(middleware.RequireAuth(systemPool, cfg))
	h.Register(authed)

	actions := r.Group("/api/v1")
	h.RegisterActions(actions)
	return r
}

func authedRequest(t *testing.T, cfg *config.Config, clerkUserID, method, path string, body any) *http.Request {
	t.Helper()
	token, err := devtoken.Sign(cfg.DevTokenSecret.Expose(), clerkUserID)
	if err != nil {
		t.Fatalf("sign dev token: %v", err)
	}
	req := jsonRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func jsonRequest(method, path string, body any) *http.Request {
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
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

func uuidFromPg(id pgtype.UUID) uuid.UUID { return uuid.UUID(id.Bytes) }

func TestApprovalsHandler_FullLifecycle(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := &config.Config{AppEnv: "development", DevTokenSecret: secret.Value("test-dev-token-secret"), PushActionTokenSecret: secret.Value("test-push-secret")}
	tokens := notify.NewActionTokenSigner(cfg.PushActionTokenSecret)

	registry, err := coremcp.NewRegistry(context.Background(), map[string]*gomcp.Server{"fake": fakeDestructiveToolServer()})
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	gateway := coremcp.NewGateway(appPool, make([]byte, 32), registry, nil)
	engine := graph.NewEngine(appPool)

	// A *MockChatClient tracks call index internally (one canned response
	// per Send() call), but Launcher resolves a chat client twice per run
	// — once for Launch's first leg, once for Resume's second — and each
	// approvalFixture's org is only used by exactly one run. Keying by
	// org_id (not building a fresh client per resolve call) is what lets
	// the *same* client, and so the *same* response sequence, answer both
	// legs — the identical structural fix MOCK_LLM_MODE's own
	// approvalScenarioClient needed for the Engine.Resume boundary (see
	// founderstack-api-go/CLAUDE.md's "Agent Execution Engine" section).
	var mockClientsMu sync.Mutex
	mockClients := map[[16]byte]*llm.MockChatClient{}
	mockResolver := func(ctx context.Context, appPool *pgxpool.Pool, encryptionKey []byte, orgID pgtype.UUID, provider llm.ProviderID, model string) (llm.ChatClient, error) {
		mockClientsMu.Lock()
		defer mockClientsMu.Unlock()
		if client, ok := mockClients[orgID.Bytes]; ok {
			return client, nil
		}
		client := llm.NewMockChatClient(
			llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "call_0", Name: "fake.delete_thing", Args: json.RawMessage(`{"id":"abc"}`)}}, StopReason: llm.StopReasonToolUse},
			llm.ChatResponse{Content: "Deleted as requested.", StopReason: llm.StopReasonEndTurn},
		)
		mockClients[orgID.Bytes] = client
		return client, nil
	}
	launcher := graph.NewLauncherWithResolver(engine, appPool, make([]byte, 32), registry, gateway, nil, mockResolver)
	router := testRouter(t, systemPool, appPool, cfg, launcher, tokens)

	t.Run("reject requires a non-empty reason", func(t *testing.T) {
		fx := newApprovalFixture(t, systemPool, appPool)
		_, approvalID := suspendNewRun(t, systemPool, appPool, fx, launcher)

		req := authedRequest(t, cfg, fx.approverClerkID, http.MethodPost, "/api/v1/approvals/"+idString(approvalID)+"/reject", map[string]any{"reason": ""})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("approve rejected when caller lacks can_approve_workflows", func(t *testing.T) {
		fx := newApprovalFixture(t, systemPool, appPool)
		_, approvalID := suspendNewRun(t, systemPool, appPool, fx, launcher)

		req := authedRequest(t, cfg, fx.nonApproverClerkUserID, http.MethodPost, "/api/v1/approvals/"+idString(approvalID)+"/approve", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("approve rejected with 403 AGENTS_PAUSED", func(t *testing.T) {
		fx := newApprovalFixture(t, systemPool, appPool)
		_, approvalID := suspendNewRun(t, systemPool, appPool, fx, launcher)
		if _, err := systemPool.Exec(context.Background(), "update organizations set agents_paused = true where id = $1", fx.orgID); err != nil {
			t.Fatal(err)
		}

		req := authedRequest(t, cfg, fx.approverClerkID, http.MethodPost, "/api/v1/approvals/"+idString(approvalID)+"/approve", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
		}
		var env apiEnvelope
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if env.Error.Code != "AGENTS_PAUSED" {
			t.Fatalf("error code = %q, want AGENTS_PAUSED", env.Error.Code)
		}
	})

	t.Run("approve happy path resumes the run", func(t *testing.T) {
		fx := newApprovalFixture(t, systemPool, appPool)
		runID, approvalID := suspendNewRun(t, systemPool, appPool, fx, launcher)

		req := authedRequest(t, cfg, fx.approverClerkID, http.MethodPost, "/api/v1/approvals/"+idString(approvalID)+"/approve", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}

		deadline := time.Now().Add(5 * time.Second)
		var status string
		for time.Now().Before(deadline) {
			if err := systemPool.QueryRow(context.Background(), "select status from workflow_runs where id = $1", runID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status == "completed" {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if status != "completed" {
			t.Fatalf("run status after approve = %q, want completed", status)
		}

		var decision string
		if err := systemPool.QueryRow(context.Background(), "select status from approvals where id = $1", approvalID).Scan(&decision); err != nil {
			t.Fatal(err)
		}
		if decision != "approved" {
			t.Fatalf("approvals.status = %q, want approved", decision)
		}
	})

	t.Run("approving twice the second time is rejected", func(t *testing.T) {
		fx := newApprovalFixture(t, systemPool, appPool)
		_, approvalID := suspendNewRun(t, systemPool, appPool, fx, launcher)

		first := authedRequest(t, cfg, fx.approverClerkID, http.MethodPost, "/api/v1/approvals/"+idString(approvalID)+"/approve", nil)
		router.ServeHTTP(httptest.NewRecorder(), first)

		second := authedRequest(t, cfg, fx.approverClerkID, http.MethodPost, "/api/v1/approvals/"+idString(approvalID)+"/approve", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, second)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("cross-org approval is not found", func(t *testing.T) {
		fxA := newApprovalFixture(t, systemPool, appPool)
		_, approvalID := suspendNewRun(t, systemPool, appPool, fxA, launcher)
		fxB := newApprovalFixture(t, systemPool, appPool)

		req := authedRequest(t, cfg, fxB.approverClerkID, http.MethodPost, "/api/v1/approvals/"+idString(approvalID)+"/approve", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("action token approves without any Authorization header", func(t *testing.T) {
		fx := newApprovalFixture(t, systemPool, appPool)
		runID, approvalID := suspendNewRun(t, systemPool, appPool, fx, launcher)
		token := tokens.Sign(uuidFromPg(approvalID), uuidFromPg(fx.approverUserID), time.Now().Add(time.Hour))

		req := jsonRequest(http.MethodPost, "/api/v1/approvals/"+idString(approvalID)+"/approve?action_token="+token, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}

		deadline := time.Now().Add(5 * time.Second)
		var status string
		for time.Now().Before(deadline) {
			if err := systemPool.QueryRow(context.Background(), "select status from workflow_runs where id = $1", runID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status == "completed" {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if status != "completed" {
			t.Fatalf("run status after action-token approve = %q, want completed", status)
		}
	})

	t.Run("expired or tampered action token is rejected", func(t *testing.T) {
		fx := newApprovalFixture(t, systemPool, appPool)
		_, approvalID := suspendNewRun(t, systemPool, appPool, fx, launcher)
		expired := tokens.Sign(uuidFromPg(approvalID), uuidFromPg(fx.approverUserID), time.Now().Add(-time.Minute))

		req := jsonRequest(http.MethodPost, "/api/v1/approvals/"+idString(approvalID)+"/approve?action_token="+expired, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("no credential at all is rejected", func(t *testing.T) {
		fx := newApprovalFixture(t, systemPool, appPool)
		_, approvalID := suspendNewRun(t, systemPool, appPool, fx, launcher)

		req := jsonRequest(http.MethodPost, "/api/v1/approvals/"+idString(approvalID)+"/approve", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
		}
	})
}

func idString(id pgtype.UUID) string {
	u := uuidFromPg(id)
	return u.String()
}

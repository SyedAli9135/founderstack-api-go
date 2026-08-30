//go:build integration

package workflows

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/founderstack/api/internal/core/graph"
	"github.com/founderstack/api/internal/core/integrations"
	"github.com/founderstack/api/internal/core/llm"
	coremcp "github.com/founderstack/api/internal/core/mcp"
)

// testAppPool is this file's own addition to the package's shared test
// scaffolding — scheduler_integration_test.go already defines
// testSystemPool/randSuffix (reused here), but nothing in workflow 8's
// scheduler needed the RLS-scoped app_user pool, so testAppPool didn't
// exist yet.
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

// suspendedApproval builds a fresh org/agent/workflow/run through a real
// graph.Launcher (fake destructive tool, MockChatClient) and polls until
// it's genuinely awaiting_approval with a real approvals row — then
// back-dates that row's expires_at into the past so expireApprovals has
// something to act on without a real 24h wait, the same "manufacture the
// edge condition live" approach this codebase's mock scenarios already use.
func suspendedApproval(t *testing.T, systemPool, appPool *pgxpool.Pool, launcher *graph.Launcher) (orgID, runID, approvalID pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	suffix := randSuffix(t)

	if err := systemPool.QueryRow(ctx,
		`insert into organizations (clerk_org_id, name, slug, llm_provider) values ($1, 'Expiry Test Org', $2, 'anthropic') returning id`,
		"org_expiry_test_"+suffix, "expiry-test-"+suffix,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	t.Cleanup(func() { _, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID) })

	policyJSON := []byte(`{"allowed_tools":["fake.delete_thing"]}`)
	var agentID pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into agents (org_id, name, slug, system_prompt, model, policy_scope) values ($1, 'Expiry Test Agent', $2, 'test', 'test-model', $3) returning id`,
		orgID, "expiry-test-agent-"+suffix, policyJSON,
	).Scan(&agentID); err != nil {
		t.Fatalf("insert test agent: %v", err)
	}

	var workflowID pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into workflows (org_id, agent_id, name, trigger_type, graph_definition) values ($1, $2, 'Expiry Test Workflow', 'manual', '{}'::jsonb) returning id`,
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

	if err := systemPool.QueryRow(ctx,
		`insert into workflow_runs (workflow_id, org_id, status) values ($1, $2, 'pending') returning id`,
		workflowID, orgID,
	).Scan(&runID); err != nil {
		t.Fatalf("insert test workflow_run: %v", err)
	}

	launcher.Launch(orgID.Bytes, agentID.Bytes, workflowID.Bytes, runID.Bytes, "delete abc")

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
			// Back-date expires_at — 24h is far too long for a test to
			// actually wait, and this codebase's own mock-scenario
			// testing already establishes the pattern of manufacturing
			// the edge condition live rather than mocking the clock.
			if _, err := systemPool.Exec(ctx, "update approvals set expires_at = now() - interval '1 minute' where id = $1", approvalID); err != nil {
				t.Fatalf("back-date approval expiry: %v", err)
			}
			return orgID, runID, approvalID
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run never reached awaiting_approval within the test deadline")
	return
}

func TestExpireApprovals_ExpiresAndResumesTheRun(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)

	registry, err := coremcp.NewRegistry(context.Background(), map[string]*gomcp.Server{"fake": fakeDestructiveToolServer()})
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	gateway := coremcp.NewGateway(appPool, make([]byte, 32), registry, nil)
	engine := graph.NewEngine(appPool)
	mockResolver := func(ctx context.Context, appPool *pgxpool.Pool, encryptionKey []byte, orgID pgtype.UUID, provider llm.ProviderID, model string) (llm.ChatClient, error) {
		// Only Launch's first leg (the suspend) ever calls Send here —
		// expiry rejects the run rather than resuming it toward a second
		// model call, so one canned response is enough.
		return llm.NewMockChatClient(
			llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "call_0", Name: "fake.delete_thing", Args: json.RawMessage(`{"id":"abc"}`)}}, StopReason: llm.StopReasonToolUse},
		), nil
	}
	launcher := graph.NewLauncherWithResolver(engine, appPool, make([]byte, 32), registry, gateway, nil, mockResolver)

	_, runID, approvalID := suspendedApproval(t, systemPool, appPool, launcher)

	expireApprovals(context.Background(), systemPool, launcher)

	var approvalStatus string
	if err := systemPool.QueryRow(context.Background(), "select status from approvals where id = $1", approvalID).Scan(&approvalStatus); err != nil {
		t.Fatal(err)
	}
	if approvalStatus != "expired" {
		t.Fatalf("approvals.status = %q, want expired", approvalStatus)
	}

	var decisionUserID pgtype.UUID
	var reason *string
	if err := systemPool.QueryRow(context.Background(), "select user_id, reason from approval_decisions where approval_id = $1", approvalID).Scan(&decisionUserID, &reason); err != nil {
		t.Fatalf("fetch approval_decisions row: %v", err)
	}
	if decisionUserID.Valid {
		t.Fatalf("approval_decisions.user_id = %v, want NULL for a system expiry", decisionUserID)
	}
	if reason == nil || *reason == "" {
		t.Fatal("approval_decisions.reason not set")
	}

	// The real point of this job: workflow_runs must actually leave
	// awaiting_approval, not just have its approvals row flipped — see
	// RunApprovalExpiryJob's doc comment for why this was a real gap.
	deadline := time.Now().Add(5 * time.Second)
	var runStatus string
	for time.Now().Before(deadline) {
		if err := systemPool.QueryRow(context.Background(), "select status from workflow_runs where id = $1", runID).Scan(&runStatus); err != nil {
			t.Fatal(err)
		}
		if runStatus == "completed" || runStatus == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if runStatus != "completed" {
		t.Fatalf("workflow_runs.status after expiry = %q, want completed (a rejected approval still finishes the run — reporterNode composes a stopped-not-approved summary)", runStatus)
	}
}

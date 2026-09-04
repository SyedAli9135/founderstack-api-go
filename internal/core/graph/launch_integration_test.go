//go:build integration

package graph

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/founderstack/api/internal/core/integrations"
	"github.com/founderstack/api/internal/core/llm"
	coremcp "github.com/founderstack/api/internal/core/mcp"
	"github.com/founderstack/api/internal/pkg/vault"
)

// launchFixture is the org/agent/workflow/run/BYOK-key/MCP-connection
// chain Launcher needs — a superset of testWorkflowRun's (engine_integration_test.go)
// fixture, since Launcher additionally resolves a real agent row, an
// org's llm_provider, and a BYOK key, none of which the bare engine
// tests needed.
type launchFixture struct {
	orgID, agentID, workflowID, runID uuid.UUID
	encKey                            []byte
}

func newLaunchFixture(t *testing.T, systemPool *pgxpool.Pool, policyScope map[string]any, provider string, hasKey bool) launchFixture {
	t.Helper()
	ctx := context.Background()
	suffix := randSuffix(t)

	var orgPg pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into organizations (clerk_org_id, name, slug, llm_provider) values ($1, 'Launch Test Org', $2, $3) returning id`,
		"org_launch_test_"+suffix, "launch-test-"+suffix, provider,
	).Scan(&orgPg); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgPg)
	})

	policyJSON, err := json.Marshal(policyScope)
	if err != nil {
		t.Fatal(err)
	}
	var agentPg pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into agents (org_id, name, slug, system_prompt, model, policy_scope) values ($1, 'Launch Test Agent', $2, 'You are a test agent.', 'test-model', $3) returning id`,
		orgPg, "launch-test-agent-"+suffix, policyJSON,
	).Scan(&agentPg); err != nil {
		t.Fatalf("insert test agent: %v", err)
	}

	var workflowPg pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into workflows (org_id, agent_id, name, trigger_type, graph_definition) values ($1, $2, 'Launch Test Workflow', 'manual', '{}'::jsonb) returning id`,
		orgPg, agentPg,
	).Scan(&workflowPg); err != nil {
		t.Fatalf("insert test workflow: %v", err)
	}

	var runPg pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into workflow_runs (workflow_id, org_id, status) values ($1, $2, 'pending') returning id`,
		workflowPg, orgPg,
	).Scan(&runPg); err != nil {
		t.Fatalf("insert test workflow_run: %v", err)
	}

	encKey := make([]byte, 32)
	if _, err := rand.Read(encKey); err != nil {
		t.Fatal(err)
	}
	if hasKey {
		encrypted, err := vault.Encrypt("fake-key-material", encKey)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := systemPool.Exec(ctx,
			`insert into api_key_registry (org_id, provider, key_prefix, encrypted_key, kms_key_id, is_valid)
			 values ($1, $2, 'fake-...', $3, 'local-aes-gcm', true)`,
			orgPg, provider, encrypted,
		); err != nil {
			t.Fatalf("insert test key: %v", err)
		}
	}

	return launchFixture{
		orgID: uuid.UUID(orgPg.Bytes), agentID: uuid.UUID(agentPg.Bytes),
		workflowID: uuid.UUID(workflowPg.Bytes), runID: uuid.UUID(runPg.Bytes), encKey: encKey,
	}
}

// mockChatClientResolver ignores the real provider/model and always
// returns client — the ChatClientResolver test seam Launcher's
// NewLauncherWithResolver exists for.
func mockChatClientResolver(client llm.ChatClient) ChatClientResolver {
	return func(ctx context.Context, appPool *pgxpool.Pool, encryptionKey []byte, orgID pgtype.UUID, provider llm.ProviderID, model string) (llm.ChatClient, error) {
		return client, nil
	}
}

func TestLauncher_PreflightBlocksWhenAgentsPaused(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	fx := newLaunchFixture(t, systemPool, map[string]any{"allowed_tools": []string{"fake.get_data"}}, "anthropic", true)
	if _, err := systemPool.Exec(context.Background(), "update organizations set agents_paused = true where id = $1", pgtype.UUID{Bytes: fx.orgID, Valid: true}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(appPool)
	launcher := NewLauncherWithResolver(engine, appPool, fx.encKey, nil, nil, nil, mockChatClientResolver(nil))

	err := launcher.Preflight(context.Background(), pgtype.UUID{Bytes: fx.orgID, Valid: true})
	if !errors.Is(err, ErrAgentsPaused) {
		t.Fatalf("Preflight() error = %v, want ErrAgentsPaused", err)
	}
}

func TestLauncher_PreflightBlocksWhenNoBYOKKey(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	fx := newLaunchFixture(t, systemPool, map[string]any{"allowed_tools": []string{"fake.get_data"}}, "anthropic", false) // hasKey=false

	engine := NewEngine(appPool)
	launcher := NewLauncherWithResolver(engine, appPool, fx.encKey, nil, nil, nil, mockChatClientResolver(nil))

	err := launcher.Preflight(context.Background(), pgtype.UUID{Bytes: fx.orgID, Valid: true})
	if !errors.Is(err, ErrNoBYOKKey) {
		t.Fatalf("Preflight() error = %v, want ErrNoBYOKKey", err)
	}
}

func TestLauncher_PreflightPassesWithValidKeyAndNotPaused(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	fx := newLaunchFixture(t, systemPool, map[string]any{"allowed_tools": []string{"fake.get_data"}}, "anthropic", true)

	engine := NewEngine(appPool)
	launcher := NewLauncherWithResolver(engine, appPool, fx.encKey, nil, nil, nil, mockChatClientResolver(nil))

	if err := launcher.Preflight(context.Background(), pgtype.UUID{Bytes: fx.orgID, Valid: true}); err != nil {
		t.Fatalf("Preflight() error = %v, want nil", err)
	}
}

func TestLauncher_LaunchRunsToCompletionAndFinalizes(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	fx := newLaunchFixture(t, systemPool, map[string]any{"allowed_tools": []string{"fake.get_data"}}, "anthropic", true)

	if err := integrations.SaveConnection(context.Background(), appPool, fx.encKey, pgtype.UUID{Bytes: fx.orgID, Valid: true}, "fake", "Fake", "manual", "connected",
		integrations.Token{AccessToken: "fake-token"},
	); err != nil {
		t.Fatalf("save fake connection: %v", err)
	}
	registry, err := coremcp.NewRegistry(context.Background(), map[string]*gomcp.Server{"fake": fakeToolServer()})
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	gateway := coremcp.NewGateway(appPool, fx.encKey, registry, nil)

	mock := llm.NewMockChatClient(
		llm.ChatResponse{
			ToolCalls:  []llm.ToolCall{{ID: "call_0", Name: "fake.get_data", Args: json.RawMessage(`{"query":"x"}`)}},
			StopReason: llm.StopReasonToolUse,
		},
		llm.ChatResponse{Content: "All done.", StopReason: llm.StopReasonEndTurn, Usage: llm.TokenUsage{InputTokens: 10, OutputTokens: 5}},
	)

	engine := NewEngine(appPool)
	launcher := NewLauncherWithResolver(engine, appPool, fx.encKey, registry, gateway, nil, mockChatClientResolver(mock))

	if err := launcher.Preflight(context.Background(), pgtype.UUID{Bytes: fx.orgID, Valid: true}); err != nil {
		t.Fatalf("Preflight() error = %v, want nil", err)
	}
	launcher.Launch(fx.orgID, fx.agentID, fx.workflowID, fx.runID, "look something up")

	// Launch is async (its own detached goroutine) — poll briefly for the
	// terminal status rather than assuming it's instantaneous. Real,
	// genuinely racy CI flake caught here: Engine.runFrom's own checkpoint
	// writes status='completed' *before* engine.Run returns, but
	// Launcher.run only calls finalizeIfTerminal (which writes output/
	// token counts/duration) *after* engine.Run returns — two separate
	// writes, by design (Engine owns status, Launcher owns the summary).
	// A poll loop that breaks on status=="completed" alone can win that
	// race and read output as still nil. Wait for output to be populated
	// too, not just the status flip.
	var row struct {
		status               string
		output               *string
		inputTok, outputTok  int32
		startedAt, completed *time.Time
		durationMs           *int32
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		// systemPool (BYPASSRLS), not appPool — a direct query against
		// app_user with no tenant.WithTx around it sees zero rows, not
		// this run's row, per this codebase's RLS design.
		err := systemPool.QueryRow(context.Background(),
			`select status, output, input_tokens, output_tokens, started_at, completed_at, duration_ms
			 from workflow_runs where id = $1`,
			pgtype.UUID{Bytes: fx.runID, Valid: true},
		).Scan(&row.status, &row.output, &row.inputTok, &row.outputTok, &row.startedAt, &row.completed, &row.durationMs)
		if err != nil {
			t.Fatalf("query run row: %v", err)
		}
		if row.status == "completed" && row.output != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if row.status != "completed" {
		t.Fatalf("status = %q, want completed (Launch never finished within the test deadline)", row.status)
	}
	if row.output == nil || *row.output != "All done." {
		t.Fatalf("output = %v, want %q", row.output, "All done.")
	}
	if row.inputTok != 10 || row.outputTok != 5 {
		t.Fatalf("token counts = (%d, %d), want (10, 5)", row.inputTok, row.outputTok)
	}
	if row.startedAt == nil || row.completed == nil {
		t.Fatal("expected started_at/completed_at to both be set")
	}
	if row.durationMs == nil {
		t.Fatal("expected duration_ms to be set")
	}
}

// TestLauncher_LaunchAccruesHoursSaved covers the other half of workflow
// 11's schema gap: workflow_runs.hours_saved/organizations.total_hours_saved
// didn't exist before this workflow, and nothing computed either. The test
// fixture's workflow never sets estimated_manual_minutes, so this exercises
// the 15-minute (0.25h) default specifically.
func TestLauncher_LaunchAccruesHoursSaved(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	fx := newLaunchFixture(t, systemPool, map[string]any{"allowed_tools": []string{"fake.get_data"}}, "anthropic", true)

	var orgHoursBefore float64
	if err := systemPool.QueryRow(context.Background(), "select total_hours_saved from organizations where id = $1", pgtype.UUID{Bytes: fx.orgID, Valid: true}).Scan(&orgHoursBefore); err != nil {
		t.Fatal(err)
	}

	if err := integrations.SaveConnection(context.Background(), appPool, fx.encKey, pgtype.UUID{Bytes: fx.orgID, Valid: true}, "fake", "Fake", "manual", "connected",
		integrations.Token{AccessToken: "fake-token"},
	); err != nil {
		t.Fatalf("save fake connection: %v", err)
	}
	registry, err := coremcp.NewRegistry(context.Background(), map[string]*gomcp.Server{"fake": fakeToolServer()})
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	gateway := coremcp.NewGateway(appPool, fx.encKey, registry, nil)
	mock := llm.NewMockChatClient(llm.ChatResponse{Content: "Done.", StopReason: llm.StopReasonEndTurn})

	engine := NewEngine(appPool)
	launcher := NewLauncherWithResolver(engine, appPool, fx.encKey, registry, gateway, nil, mockChatClientResolver(mock))
	if err := launcher.Preflight(context.Background(), pgtype.UUID{Bytes: fx.orgID, Valid: true}); err != nil {
		t.Fatalf("Preflight() error = %v, want nil", err)
	}
	launcher.Launch(fx.orgID, fx.agentID, fx.workflowID, fx.runID, "do a thing")

	var hoursSaved *float64
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := systemPool.QueryRow(context.Background(), "select hours_saved from workflow_runs where id = $1", pgtype.UUID{Bytes: fx.runID, Valid: true}).Scan(&hoursSaved); err != nil {
			t.Fatalf("query run hours_saved: %v", err)
		}
		if hoursSaved != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if hoursSaved == nil {
		t.Fatal("workflow_runs.hours_saved was never set within the test deadline")
	}
	if *hoursSaved != 0.25 {
		t.Fatalf("hours_saved = %v, want 0.25 (15-minute default, no estimated_manual_minutes set)", *hoursSaved)
	}

	var orgHoursAfter float64
	if err := systemPool.QueryRow(context.Background(), "select total_hours_saved from organizations where id = $1", pgtype.UUID{Bytes: fx.orgID, Valid: true}).Scan(&orgHoursAfter); err != nil {
		t.Fatal(err)
	}
	if orgHoursAfter-orgHoursBefore != 0.25 {
		t.Fatalf("organizations.total_hours_saved delta = %v, want 0.25", orgHoursAfter-orgHoursBefore)
	}
}

func TestLauncher_LaunchMarksFailedWhenAgentHasNoModel(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	fx := newLaunchFixture(t, systemPool, map[string]any{"allowed_tools": []string{"fake.get_data"}}, "anthropic", true)
	if _, err := systemPool.Exec(context.Background(), "update agents set model = null where id = $1", pgtype.UUID{Bytes: fx.agentID, Valid: true}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(appPool)
	launcher := NewLauncherWithResolver(engine, appPool, fx.encKey, nil, nil, nil, mockChatClientResolver(llm.NewMockChatClient()))
	launcher.Launch(fx.orgID, fx.agentID, fx.workflowID, fx.runID, "do something")

	var status string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := systemPool.QueryRow(context.Background(), "select status from workflow_runs where id = $1", pgtype.UUID{Bytes: fx.runID, Valid: true}).Scan(&status); err != nil {
			t.Fatalf("query run status: %v", err)
		}
		if status == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != "failed" {
		t.Fatalf("status = %q, want failed (no model configured should fail fast, not hang at pending)", status)
	}
}

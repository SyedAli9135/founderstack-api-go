//go:build integration

package graph

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// --- shared test scaffolding: org/agent/workflow/workflow_run fixtures,
// same shape as internal/core/documents/processor_integration_test.go's
// testAppPool/testSystemPool/testOrg — this file's assertions read back
// through systemPool (BYPASSRLS) purely for convenience; the engine code
// under test always goes through appPool via tenant.WithTx. ---

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

// testWorkflowRun creates the full org -> agent -> workflow -> workflow_run
// fixture chain a real run needs (workflow_runs.workflow_id is NOT NULL),
// and returns both the org and the fresh pending run's IDs as uuid.UUID
// (RunState's own type, not pgtype.UUID) for direct use building a RunState.
func testWorkflowRun(t *testing.T, systemPool *pgxpool.Pool) (orgID, agentID, runID uuid.UUID) {
	t.Helper()
	suffix := randSuffix(t)
	ctx := context.Background()

	var orgPg pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into organizations (clerk_org_id, name, slug) values ($1, 'Graph Test Org', $2) returning id`,
		"org_graph_test_"+suffix, "graph-test-"+suffix,
	).Scan(&orgPg); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgPg)
	})

	var agentPg pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into agents (org_id, name, slug, system_prompt) values ($1, 'Graph Test Agent', $2, 'test') returning id`,
		orgPg, "graph-test-agent-"+suffix,
	).Scan(&agentPg); err != nil {
		t.Fatalf("insert test agent: %v", err)
	}

	var workflowPg pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into workflows (org_id, agent_id, name, trigger_type, graph_definition) values ($1, $2, 'Graph Test Workflow', 'manual', '{}'::jsonb) returning id`,
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

	return uuid.UUID(orgPg.Bytes), uuid.UUID(agentPg.Bytes), uuid.UUID(runPg.Bytes)
}

func fetchRunRow(t *testing.T, systemPool *pgxpool.Pool, runID uuid.UUID) (status string, currentNode *string, hasCheckpoint bool) {
	t.Helper()
	var checkpointState []byte
	err := systemPool.QueryRow(context.Background(),
		`select status, current_node, checkpoint_state from workflow_runs where id = $1`,
		pgtype.UUID{Bytes: runID, Valid: true},
	).Scan(&status, &currentNode, &checkpointState)
	if err != nil {
		t.Fatalf("fetch workflow_run row: %v", err)
	}
	return status, currentNode, checkpointState != nil
}

// --- tests ---

func TestEngine_RunWalksNodesAndCheckpointsToCompletion(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	orgID, agentID, runID := testWorkflowRun(t, systemPool)

	engine := NewEngine(appPool)
	state := &RunState{OrgID: orgID, AgentID: agentID, WorkflowRunID: runID, Input: "do the thing"}

	var visited []string
	nodes := Nodes{
		"planner": func(ctx context.Context, s *RunState) (NodeName, error) {
			visited = append(visited, "planner")
			return "executor", nil
		},
		"executor": func(ctx context.Context, s *RunState) (NodeName, error) {
			visited = append(visited, "executor")
			s.ToolCallCount++
			s.Output = "done"
			return NodeComplete, nil
		},
	}

	if err := engine.Run(context.Background(), nodes, state, "planner"); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := []string{"planner", "executor"}; len(visited) != 2 || visited[0] != got[0] || visited[1] != got[1] {
		t.Fatalf("expected nodes visited in order %v, got %v", got, visited)
	}

	status, currentNode, hasCheckpoint := fetchRunRow(t, systemPool, runID)
	if status != "completed" {
		t.Fatalf("expected status=completed, got %q", status)
	}
	if currentNode == nil || *currentNode != "" {
		t.Fatalf("expected current_node='' (NodeComplete) on completion, got %v", currentNode)
	}
	if !hasCheckpoint {
		t.Fatal("expected checkpoint_state to be persisted")
	}
}

func TestEngine_SuspendsAtApprovalGateThenResumes(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	orgID, agentID, runID := testWorkflowRun(t, systemPool)

	engine := NewEngine(appPool)
	state := &RunState{OrgID: orgID, AgentID: agentID, WorkflowRunID: runID, Input: "pay the invoice"}

	nodes := Nodes{
		"executor": func(ctx context.Context, s *RunState) (NodeName, error) {
			s.ToolCallCount++
			return NodeAwaitingApproval, nil
		},
		"approval_gate": func(ctx context.Context, s *RunState) (NodeName, error) {
			// Resume() lands back here (currentNode == NodeAwaitingApproval
			// == "approval_gate"); the approval decision is already on
			// s.Approval by the time this runs.
			if s.Approval == nil {
				return "", errors.New("expected Approval to be set by Resume")
			}
			if !s.Approval.Approved {
				return NodeComplete, nil
			}
			return "reporter", nil
		},
		"reporter": func(ctx context.Context, s *RunState) (NodeName, error) {
			s.Output = "approved and executed"
			return NodeComplete, nil
		},
	}

	// First leg: suspends at the approval gate, Run returns nil (not an
	// error — a suspended run isn't a failed run).
	if err := engine.Run(context.Background(), nodes, state, "executor"); err != nil {
		t.Fatalf("Run (first leg) returned error: %v", err)
	}

	status, currentNode, _ := fetchRunRow(t, systemPool, runID)
	if status != "awaiting_approval" {
		t.Fatalf("expected status=awaiting_approval after suspending, got %q", status)
	}
	if currentNode == nil || *currentNode != "approval_gate" {
		t.Fatalf("expected current_node=approval_gate, got %v", currentNode)
	}

	// Second leg: a human approves — Resume reloads the checkpoint (a fresh
	// RunState value, not the one still held in this test's memory) and
	// continues from approval_gate through to completion.
	if err := engine.Resume(context.Background(), nodes, orgID, runID, ResumeData{Approved: true}); err != nil {
		t.Fatalf("Resume returned error: %v", err)
	}

	status, currentNode, _ = fetchRunRow(t, systemPool, runID)
	if status != "completed" {
		t.Fatalf("expected status=completed after resume, got %q", status)
	}
	if currentNode == nil || *currentNode != "" {
		t.Fatalf("expected current_node='' after completion, got %v", currentNode)
	}
}

func TestEngine_ResumeRejectsRunWithNoCheckpoint(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	orgID, _, runID := testWorkflowRun(t, systemPool) // never Run(), so no checkpoint exists

	engine := NewEngine(appPool)
	err := engine.Resume(context.Background(), Nodes{}, orgID, runID, ResumeData{Approved: true})
	if err == nil {
		t.Fatal("expected an error resuming a run that never checkpointed")
	}
}

func TestEngine_CancelStopsAnInFlightRun(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	orgID, agentID, runID := testWorkflowRun(t, systemPool)

	engine := NewEngine(appPool)
	state := &RunState{OrgID: orgID, AgentID: agentID, WorkflowRunID: runID}

	nodeStarted := make(chan struct{})
	nodes := Nodes{
		"executor": func(ctx context.Context, s *RunState) (NodeName, error) {
			close(nodeStarted)
			<-ctx.Done() // blocks until Cancel() fires
			return "", ctx.Err()
		},
	}

	runErr := make(chan error, 1)
	go func() {
		runErr <- engine.Run(context.Background(), nodes, state, "executor")
	}()

	select {
	case <-nodeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("node never started")
	}

	if !engine.Cancel(runID) {
		t.Fatal("Cancel returned false for a run that should be in-flight")
	}

	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run never returned after Cancel")
	}

	status, _, _ := fetchRunRow(t, systemPool, runID)
	if status != "cancelled" {
		t.Fatalf("expected status=cancelled, got %q", status)
	}

	if engine.Cancel(runID) {
		t.Fatal("Cancel should return false once the run has already finished")
	}
}

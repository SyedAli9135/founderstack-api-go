//go:build integration

package graph

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/founderstack/api/internal/core/llm"
)

func TestWriteApprovalGate_InsertsRealApprovalRow(t *testing.T) {
	mock := llm.NewMockChatClient(
		llm.ChatResponse{
			ToolCalls:  []llm.ToolCall{{ID: "call_0", Name: "fake.delete_thing", Args: json.RawMessage(`{"id":"abc"}`)}},
			StopReason: llm.StopReasonToolUse,
		},
		llm.ChatResponse{Content: "Deleted as requested.", StopReason: llm.StopReasonEndTurn},
	)
	deps, orgID, agentID, runID := nodesTestDeps(t, mock, []string{"fake.delete_thing"})
	nodes := BuildNodes(deps)
	state := &RunState{OrgID: orgID, AgentID: agentID, WorkflowRunID: runID, Input: "delete abc"}

	if err := deps.Engine.Run(context.Background(), nodes, state, "planner"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// The run suspended — RunState.ApprovalID/PendingApprovalRiskLevel
	// must be set (writeApprovalGate's job) so engine.go's
	// EventApprovalRequired publish and the checkpoint both had something
	// real to carry.
	if state.ApprovalID.String() == "00000000-0000-0000-0000-000000000000" {
		t.Fatal("state.ApprovalID is zero — writeApprovalGate did not set it")
	}
	if state.PendingApprovalRiskLevel != "write_destructive_or_financial" {
		t.Fatalf("state.PendingApprovalRiskLevel = %q, want write_destructive_or_financial", state.PendingApprovalRiskLevel)
	}

	systemPool := testSystemPool(t)
	var status, riskLevel string
	var contextData []byte
	var expiresAt pgtype.Timestamptz
	var dbRunID pgtype.UUID
	err := systemPool.QueryRow(context.Background(),
		`select status, risk_level, context_data, expires_at, run_id from approvals where id = $1`,
		pgtype.UUID{Bytes: state.ApprovalID, Valid: true},
	).Scan(&status, &riskLevel, &contextData, &expiresAt, &dbRunID)
	if err != nil {
		t.Fatalf("fetch approvals row: %v", err)
	}
	if status != "pending" {
		t.Fatalf("approvals.status = %q, want pending", status)
	}
	if riskLevel != "write_destructive_or_financial" {
		t.Fatalf("approvals.risk_level = %q, want write_destructive_or_financial", riskLevel)
	}
	if !expiresAt.Valid || expiresAt.Time.IsZero() {
		t.Fatal("approvals.expires_at not set")
	}
	var calls []llm.ToolCall
	if err := json.Unmarshal(contextData, &calls); err != nil {
		t.Fatalf("unmarshal approvals.context_data: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "fake.delete_thing" {
		t.Fatalf("approvals.context_data = %s, want the 1 pending tool call", contextData)
	}

	// state.ApprovalID must survive a checkpoint/reload round trip — it's
	// exactly what engine.go's NodeAwaitingApproval case reads to build
	// the SSE event's Data payload, and what a real approve/reject
	// handler resolving this run later still needs.
	reloaded, _, err := loadCheckpoint(context.Background(), deps.AppPool, orgID, runID)
	if err != nil {
		t.Fatalf("loadCheckpoint: %v", err)
	}
	if reloaded.ApprovalID != state.ApprovalID {
		t.Fatalf("reloaded ApprovalID = %v, want %v", reloaded.ApprovalID, state.ApprovalID)
	}
}

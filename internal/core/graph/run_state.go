// Package graph is the workflow-9 execution engine: a hand-rolled,
// Postgres-checkpointed state machine standing in for LangGraph's
// planner -> RAG -> executor -> approval -> validator -> reporter sequence.
// See WORKFLOW_PLAN_GO.md's Workflow 9 harness planning notes for the full
// design (guardrails, context/loop strategy, checkpointing granularity) this
// package is built against.
package graph

import (
	"encoding/json"

	"github.com/google/uuid"

	"github.com/founderstack/api/internal/core/llm"
)

// NodeName identifies a node in the graph.
type NodeName string

// NodeComplete, returned by a node as "next", ends the run successfully.
// NodeAwaitingApproval suspends it — Engine.Resume continues from here once
// a human decides. Both are reserved names Engine itself understands;
// everything else is whatever names the caller's Nodes map uses.
const (
	NodeComplete         NodeName = ""
	NodeAwaitingApproval NodeName = "approval_gate"
)

// RunState is the full state of one workflow run — JSON-serialized into
// workflow_runs.checkpoint_state after every individual tool call, not just
// every node transition (a crash mid-node would otherwise replay from tool
// call 1 on Resume). OrgID/AgentID/WorkflowRunID are set once at the start of
// a run and never change; everything else accumulates as nodes run.
type RunState struct {
	OrgID         uuid.UUID `json:"org_id"`
	AgentID       uuid.UUID `json:"agent_id"`
	WorkflowRunID uuid.UUID `json:"workflow_run_id"`
	// AgentName is denormalized from the agent row at Launch time purely
	// so Engine's node_start/node_end events can carry it (Engine itself
	// only ever sees RunState, never the agents table) — see
	// WORKFLOW_PLAN_GO.md's Workflow 9 acceptance criteria ("Every node
	// transition emits a node_start SSE event with agent name + node type").
	AgentName string `json:"agent_name,omitempty"`

	Input        string          `json:"input"`
	PlannedTools []string        `json:"planned_tools,omitempty"`
	RAGChunks    []string        `json:"rag_chunks,omitempty"`
	ToolResults  []ToolResult    `json:"tool_results,omitempty"`
	Output       string          `json:"output,omitempty"`
	TokenUsage   TokenUsage      `json:"token_usage"`
	Approval     *ApprovalResult `json:"approval_result,omitempty"`

	// Conversation is the literal message history fed to ChatClient.Send
	// each turn — the source of truth executor_node's inner loop drives
	// off of, distinct from ToolResults (a parallel, simplified summary
	// kept for reporter_node/audit display). Rebuilt from nothing on a
	// fresh run; reloaded verbatim from the checkpoint on Resume.
	Conversation []llm.Message `json:"conversation,omitempty"`
	// PendingToolCalls holds the tool-call batch executor_node suspended
	// on when it returned NodeAwaitingApproval — the approval_gate node
	// reads this on Resume to know what to execute (or discard, on
	// rejection). Cleared once the gate resolves it.
	PendingToolCalls []llm.ToolCall `json:"pending_tool_calls,omitempty"`
	// LastToolCallBatch is a stuck-loop detector: if the model requests
	// the exact same batch of tool calls twice in a row, executor_node
	// aborts rather than burning the full max_tool_calls budget on a
	// death spiral — see the harness plan's loop-engineering notes.
	LastToolCallBatch string `json:"last_tool_call_batch,omitempty"`
	// Warnings accumulates non-fatal flags from validator_node (e.g. a
	// tool result resembling an injected instruction) — surfaced in the
	// final output, never silently dropped.
	Warnings []string `json:"warnings,omitempty"`

	// ToolCallCount/CostSoFarUSD are what the engine checks after every
	// LLM/tool call against agents.policy_scope's max_tool_calls/
	// max_cost_per_run_usd — see the harness planning notes.
	ToolCallCount int     `json:"tool_call_count"`
	CostSoFarUSD  float64 `json:"cost_so_far_usd"`
}

// ToolResult is one executed tool call's record — kept in RunState so a
// resumed run (and the reporter node) can see the full history, not just the
// latest call.
type ToolResult struct {
	ToolName string          `json:"tool_name"`
	Args     json.RawMessage `json:"args,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// TokenUsage accumulates across every LLM call in the run.
type TokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	CachedTokens int `json:"cached_tokens"`
}

// ApprovalResult is set on RunState by Resume() from the human's decision —
// nil until a run has actually gone through an approval gate.
type ApprovalResult struct {
	Approved bool   `json:"approved"`
	Reason   string `json:"reason,omitempty"`
}

// ResumeData is what Engine.Resume needs from the caller (workflow 10's
// approve/reject/expire handlers) to continue a suspended run.
type ResumeData struct {
	Approved bool
	Reason   string
}

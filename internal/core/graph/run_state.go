package graph

import (
	"encoding/json"

	"github.com/google/uuid"

	"github.com/founderstack/api/internal/core/llm"
)

// NodeName identifies a node in the graph.
type NodeName string

const (
	NodeComplete         NodeName = ""
	NodeAwaitingApproval NodeName = "approval_gate"
)

// RunState is the full state of one workflow run — JSON-serialized into
// workflow_runs.checkpoint_state after every individual tool call, not just
// every node transition. OrgID/AgentID/WorkflowRunID are set once at the start of
// a run and never change; everything else accumulates as nodes run.
type RunState struct {
	OrgID         uuid.UUID `json:"org_id"`
	AgentID       uuid.UUID `json:"agent_id"`
	WorkflowRunID uuid.UUID `json:"workflow_run_id"`
	AgentName     string    `json:"agent_name,omitempty"`

	Input        string          `json:"input"`
	PlannedTools []string        `json:"planned_tools,omitempty"`
	RAGChunks    []string        `json:"rag_chunks,omitempty"`
	ToolResults  []ToolResult    `json:"tool_results,omitempty"`
	Output       string          `json:"output,omitempty"`
	TokenUsage   TokenUsage      `json:"token_usage"`
	Approval     *ApprovalResult `json:"approval_result,omitempty"`

	Conversation []llm.Message `json:"conversation,omitempty"`

	PendingToolCalls []llm.ToolCall `json:"pending_tool_calls,omitempty"`

	// ApprovalID/PendingApprovalRiskLevel are set by writeApprovalGate
	// (approvalgate.go) in the same turn PendingToolCalls is populated —
	// they identify the real `approvals` row a human decides against,
	// letting engine.go's NodeAwaitingApproval case enrich the
	// EventApprovalRequired SSE payload without itself needing a Gateway
	// dependency.
	ApprovalID               uuid.UUID `json:"approval_id,omitempty"`
	PendingApprovalRiskLevel string    `json:"pending_approval_risk_level,omitempty"`

	LastToolCallBatch string `json:"last_tool_call_batch,omitempty"`

	Warnings []string `json:"warnings,omitempty"`

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

// ResumeData is what Engine.Resume needs from the caller to continue a suspended run.
type ResumeData struct {
	Approved bool
	Reason   string
}

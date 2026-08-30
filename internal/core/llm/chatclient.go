package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Role is a conversation turn's speaker, in the generic shape every
// ChatClient implementation normalizes its provider's own role system
// into/out of.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool carries one tool's result back to the model. Every provider
	// here represents this differently on the wire (Anthropic/Gemini fold
	// it into a user-role turn, OpenAI-compatible has a real "tool" role)
	// — that mapping is each adapter's own job, not something callers of
	// ChatClient need to know.
	RoleTool Role = "tool"
)

// Message is one turn a ChatClient.Send call includes in its conversation
// history.
type Message struct {
	Role    Role
	Content string
	// ToolCalls is set on an assistant turn that requested tool
	// execution.
	ToolCalls []ToolCall
	// Name is the tool's name — required on a RoleTool message. Anthropic
	// and OpenAI-compatible match a tool result back to its call via
	// ToolCallID alone, but Gemini has no per-call ID concept and matches
	// by function name instead, so every RoleTool message carries both.
	Name string
	// ToolCallID references the ToolCall.ID this RoleTool message answers.
	ToolCallID string
	// IsError marks a RoleTool message whose tool execution failed —
	// Content is still the (error) result text to show the model.
	IsError bool
}

// ToolCall is one tool invocation the model requested. Explicit lowercase
// json tags (added for Workflow 10) — this struct now crosses the wire
// directly (graph.ApprovalRequiredData.ToolCalls, and the approvals table's
// context_data column, both consumed by founderstack-web's ApprovalCard),
// not just round-tripped through Go's own json.Marshal/Unmarshal on both
// ends of workflow_runs.checkpoint_state, where the previous default
// (capitalized field names) never actually mattered to any outside reader.
type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// ToolSchema is one tool's definition offered to the model each turn.
// InputSchema is a JSON Schema object (e.g.
// {"type":"object","properties":{...},"required":[...]}), passed through
// as-is to whichever provider — every provider here accepts a standard
// JSON Schema for tool parameters.
type ToolSchema struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// StopReason is normalized across providers. The graph engine's inner
// loop (see WORKFLOW_PLAN_GO.md's Workflow 9 "Loop engineering" notes)
// treats the absence of tool calls as the model's own stop signal, not
// something the harness guesses — StopReasonToolUse is what it checks.
type StopReason string

const (
	StopReasonEndTurn   StopReason = "end_turn"
	StopReasonToolUse   StopReason = "tool_use"
	StopReasonMaxTokens StopReason = "max_tokens"
	StopReasonOther     StopReason = "other"
)

// TokenUsage is one Send call's token accounting — accumulated into
// RunState.TokenUsage by the engine across a run.
type TokenUsage struct {
	InputTokens  int
	OutputTokens int
	CachedTokens int
}

// ChatResponse is one Send call's result.
type ChatResponse struct {
	Content    string
	ToolCalls  []ToolCall
	StopReason StopReason
	Usage      TokenUsage
}

// ChatClient is the provider-agnostic chat+tool-calling interface the
// graph engine's inner loop drives, one call per turn — the engine (not
// this interface) owns the call-tool-repeat loop, matching the decision
// to hand-roll a provider-agnostic state machine instead of using any one
// provider's SDK-bundled agentic loop (e.g. anthropic-sdk-go's Tool
// Runner). Implementations are plain net/http against each provider's
// REST API, no SDK dependency, matching this codebase's existing
// dependency-minimalism policy (see internal/core/llm/verify.go's
// verifyOpenAICompatible/verifyGemini for the same pattern already
// established for BYOK key validation).
type ChatClient interface {
	Send(ctx context.Context, systemPrompt string, messages []Message, tools []ToolSchema) (ChatResponse, error)
}

var (
	// ErrChatRejected means the provider's API was reached and rejected
	// the request outright — bad auth, malformed request. Not retryable;
	// the graph engine's stuck-loop/retry logic must not retry this.
	ErrChatRejected = errors.New("llm: provider rejected the chat request")
	// ErrChatUnavailable means the provider couldn't be reached, or
	// returned a rate-limit/server error — retryable, same
	// terminal-vs-retryable split ErrKeyRejected/ErrValidationUnavailable
	// already establish for BYOK key validation.
	ErrChatUnavailable = errors.New("llm: could not complete the chat request")
)

// chatHTTPClient is shared by every REST-based ChatClient implementation.
// No timeout here (unlike verify.go's httpClient, which validates a key
// in the request path of a founder submitting one) — a chat completion
// can legitimately take much longer than a "list models" call; the graph
// engine's own per-run wall-clock timeout is what bounds this instead.
var chatHTTPClient = &http.Client{}

// classifyChatError maps an HTTP status code + response body to
// ErrChatRejected (terminal) or ErrChatUnavailable (retryable) — shared
// by all 3 adapters below. 400/401/403 are the request or credentials
// being wrong (the founder's/agent's problem, not retryable); everything
// else (429, 5xx) is treated as transient.
func classifyChatError(statusCode int, body []byte) error {
	switch statusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: status %d: %s", ErrChatRejected, statusCode, truncateForError(body))
	default:
		return fmt.Errorf("%w: status %d: %s", ErrChatUnavailable, statusCode, truncateForError(body))
	}
}

// truncateForError keeps a provider's error body out of logs/errors at
// unbounded length — some providers echo back large request payloads in
// 400 bodies.
func truncateForError(body []byte) string {
	const max = 500
	if len(body) > max {
		return string(body[:max]) + "...(truncated)"
	}
	return string(body)
}

// requestTimeout is applied per Send call by wrapping the caller's
// context — a safety net independent of the graph engine's own run-level
// timeout, in case a caller invokes a ChatClient outside the engine
// (e.g. directly from a test or a future non-run code path).
const requestTimeout = 120 * time.Second

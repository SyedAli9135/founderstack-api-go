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
	// RoleTool carries a tool result back to the model. Each provider
	// represents this differently on the wire; mapping it is each
	// adapter's job, not something ChatClient callers need to know.
	RoleTool Role = "tool"
)

// Message is one turn in a ChatClient.Send call's conversation history.
type Message struct {
	Role      Role
	Content   string
	ToolCalls []ToolCall // set on an assistant turn that requested tool execution
	// Name is the tool's name, required on a RoleTool message: Gemini has
	// no per-call ID and matches a result to its call by name instead, so
	// every RoleTool message carries both Name and ToolCallID.
	Name       string
	ToolCallID string
	IsError    bool // marks a RoleTool message whose tool execution failed
}

// ToolCall is one tool invocation the model requested. json tags are
// explicit (lowercase) since this struct crosses the wire directly —
// graph.ApprovalRequiredData.ToolCalls and approvals.context_data.
type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// ToolSchema is one tool's definition offered to the model each turn.
// InputSchema is a JSON Schema object, passed through as-is.
type ToolSchema struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// StopReason is normalized across providers. The graph engine treats the
// absence of tool calls (StopReasonToolUse not set) as the model's own
// stop signal.
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
// graph engine's inner loop drives, one call per turn — the engine, not
// this interface, owns the call-tool-repeat loop. Implementations are
// plain net/http against each provider's REST API, no SDK dependency.
type ChatClient interface {
	Send(ctx context.Context, systemPrompt string, messages []Message, tools []ToolSchema) (ChatResponse, error)
}

var (
	// ErrChatRejected: the provider rejected the request outright (bad
	// auth, malformed request). Not retryable.
	ErrChatRejected = errors.New("llm: provider rejected the chat request")
	// ErrChatUnavailable: the provider was unreachable or rate-limited.
	// Retryable.
	ErrChatUnavailable = errors.New("llm: could not complete the chat request")
)

// chatHTTPClient has no timeout, unlike verify.go's httpClient — a chat
// completion can take much longer than a key-validation call; the graph
// engine's own per-run timeout bounds this instead.
var chatHTTPClient = &http.Client{}

// classifyChatError maps a status code to ErrChatRejected (4xx, the
// caller's problem) or ErrChatUnavailable (429/5xx, transient).
func classifyChatError(statusCode int, body []byte) error {
	switch statusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: status %d: %s", ErrChatRejected, statusCode, truncateForError(body))
	default:
		return fmt.Errorf("%w: status %d: %s", ErrChatUnavailable, statusCode, truncateForError(body))
	}
}

// truncateForError caps an error body at 500 bytes — some providers echo
// large request payloads back in 400 responses.
func truncateForError(body []byte) string {
	const max = 500
	if len(body) > max {
		return string(body[:max]) + "...(truncated)"
	}
	return string(body)
}

// requestTimeout is a safety net independent of the graph engine's own
// run-level timeout, for callers that invoke a ChatClient outside it.
const requestTimeout = 120 * time.Second

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/founderstack/api/internal/core/llm"
	coremcp "github.com/founderstack/api/internal/core/mcp"
)

// RunDeps bundles everything a run's nodes need beyond RunState itself —
// built once per run, closed over by each node function via BuildNodes.
type RunDeps struct {
	Engine       *Engine
	ChatClient   llm.ChatClient
	Gateway      *coremcp.Gateway
	Tools        ResolvedTools
	Policy       PolicyScope
	SystemPrompt string
	OrgID        pgtype.UUID
	Model        string
	AppPool      *pgxpool.Pool
}

// BuildNodes wires the 4 nodes this build step implements — planner,
// executor (the full ReAct inner loop), the approval_gate resume handler,
// and validator/reporter. RAG retrieval and the human-facing side of approval
func BuildNodes(deps RunDeps) Nodes {
	return Nodes{
		"planner":            plannerNode(deps),
		"executor":           executorNode(deps),
		NodeAwaitingApproval: approvalGateNode(deps),
		"validator":          validatorNode(deps),
		"reporter":           reporterNode(deps),
	}
}

// plannerNode seeds the conversation from the run's input and hands off
// to executor_node, which owns the actual call-model/execute-tools loop.
func plannerNode(deps RunDeps) NodeFunc {
	return func(ctx context.Context, state *RunState) (NodeName, error) {
		state.Conversation = []llm.Message{{Role: llm.RoleUser, Content: state.Input}}
		return "executor", nil
	}
}

// executorNode is the ReAct-style inner loop, execute any requested
// tool calls through the risk/policy gates, feed results back, repeat —
// bounded by policy_scope.max_tool_calls, with the model's own "no tool
// calls returned" response as the stop signal. Every tool call is
// checkpointed individually (Engine.Checkpoint), not just at this node's
// eventual return.
func executorNode(deps RunDeps) NodeFunc {
	return func(ctx context.Context, state *RunState) (NodeName, error) {
		for {
			if err := ctx.Err(); err != nil {
				return "", err
			}

			resp, err := sendWithRetry(ctx, deps.ChatClient, deps.SystemPrompt, state.Conversation, deps.Tools.Schemas)
			if err != nil {
				return "", fmt.Errorf("graph: executor chat call: %w", err)
			}
			accumulateUsage(state, deps.Model, resp.Usage)
			if err := writeCostLedgerLLMCall(ctx, deps, state, deps.Model, resp.Usage); err != nil {
				slog.Error("graph: write cost_ledger for LLM call failed", "run_id", state.WorkflowRunID, "err", err)
			}
			state.Conversation = append(state.Conversation, llm.Message{
				Role: llm.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls,
			})

			if resp.Content != "" {
				deps.Engine.Bus.Publish(Event{Type: EventReasoning, RunID: state.WorkflowRunID, Data: map[string]any{"text": resp.Content}})
			}

			if resp.StopReason != llm.StopReasonToolUse || len(resp.ToolCalls) == 0 {
				state.Output = resp.Content
				return "validator", nil
			}

			sig := toolCallBatchSignature(resp.ToolCalls)
			if sig == state.LastToolCallBatch {
				return "", fmt.Errorf("graph: stuck loop detected — model repeated the same tool call batch: %s", toolCallNames(resp.ToolCalls))
			}
			state.LastToolCallBatch = sig

			// A batch is gated as a whole: if any call in it needs
			// approval, nothing in the batch executes until a human
			// decides — including the reversible calls alongside it.
			needsApproval := false
			for _, tc := range resp.ToolCalls {
				if err := deps.Policy.CheckToolAllowed(tc.Name); err != nil {
					return "", err
				}
				if RequiresApproval(deps.Tools.RiskLevel(tc.Name)) {
					needsApproval = true
				}
			}
			if needsApproval {
				state.PendingToolCalls = resp.ToolCalls
				return NodeAwaitingApproval, nil
			}

			for _, tc := range resp.ToolCalls {
				if err := executeOneToolCall(ctx, deps, state, tc); err != nil {
					return "", err
				}
				if err := deps.Policy.CheckCaps(state); err != nil {
					return "", err
				}
			}
			// Loop back: call the model again with the tool results just
			// appended to state.Conversation.
		}
	}
}

// approvalGateNode is what Engine.Resume actually invokes (current_node
// is checkpointed as NodeAwaitingApproval == "approval_gate" when
// executor_node suspends) — not the code that decides to suspend, which
// is executor_node's own job above. Resume() has already set
// state.Approval from the human's decision by the time this runs.
func approvalGateNode(deps RunDeps) NodeFunc {
	return func(ctx context.Context, state *RunState) (NodeName, error) {
		if state.Approval == nil {
			return "", errors.New("graph: approval_gate node reached with no Approval decision set")
		}
		pending := state.PendingToolCalls
		state.PendingToolCalls = nil

		if !state.Approval.Approved {
			reason := state.Approval.Reason
			if reason == "" {
				reason = "no reason given"
			}
			state.Output = fmt.Sprintf("Run stopped: the pending action(s) were not approved. Reason: %s", reason)
			return "reporter", nil
		}

		for _, tc := range pending {
			if err := executeOneToolCall(ctx, deps, state, tc); err != nil {
				return "", err
			}
			if err := deps.Policy.CheckCaps(state); err != nil {
				return "", err
			}
		}
		return "executor", nil
	}
}

// suspiciousInstructionPatterns is a first-pass heuristic for prompt-injection guardrail:
// tool results are untrusted input flowing back into the model's context the same way any external
// content is in an agentic system. Deliberately a flag, not a hard
// block — a substring heuristic has real false positives, and the
// founder should see the warning and judge it, not have the run silently
// fail on a false alarm.
var suspiciousInstructionPatterns = []string{
	"ignore previous instructions",
	"ignore all previous instructions",
	"disregard the above",
	"disregard previous instructions",
	"new instructions:",
	"system prompt:",
	"you are now",
}

// validatorNode flags (not blocks) tool results that look like an
// injected instruction.
func validatorNode(deps RunDeps) NodeFunc {
	return func(ctx context.Context, state *RunState) (NodeName, error) {
		for _, msg := range state.Conversation {
			if msg.Role != llm.RoleTool {
				continue
			}
			lower := strings.ToLower(msg.Content)
			for _, pattern := range suspiciousInstructionPatterns {
				if strings.Contains(lower, pattern) {
					state.Warnings = append(state.Warnings, fmt.Sprintf(
						"tool %q returned content resembling an injected instruction (matched %q)", msg.Name, pattern))
					break
				}
			}
		}
		return "reporter", nil
	}
}

// reporterNode composes the final output.This just makes sure validator's
// warnings, if any, are surfaced rather than silently dropped.
func reporterNode(deps RunDeps) NodeFunc {
	return func(ctx context.Context, state *RunState) (NodeName, error) {
		if len(state.Warnings) > 0 {
			state.Output = fmt.Sprintf("⚠️ %d warning(s):\n%s\n\n%s",
				len(state.Warnings), strings.Join(state.Warnings, "\n"), state.Output)
		}
		return NodeComplete, nil
	}
}

// executeOneToolCall runs tc via the MCP gateway, appends both the
// model-facing tool-result message and the audit-facing ToolResult
// record, increments ToolCallCount, and checkpoints — used by both
// executorNode's normal loop and approvalGateNode's post-approval path,
// so both go through identical bookkeeping.
func executeOneToolCall(ctx context.Context, deps RunDeps, state *RunState, tc llm.ToolCall) error {
	service, toolName, ok := strings.Cut(tc.Name, ".")
	if !ok {
		return fmt.Errorf("graph: malformed qualified tool name %q", tc.Name)
	}

	var args map[string]any
	if len(tc.Args) > 0 {
		if err := json.Unmarshal(tc.Args, &args); err != nil {
			return fmt.Errorf("graph: unmarshal args for %s: %w", tc.Name, err)
		}
	}

	deps.Engine.Bus.Publish(Event{Type: EventToolCall, RunID: state.WorkflowRunID, Data: map[string]any{"tool": tc.Name, "args": args}})

	// Idempotency key for financial-tier calls only (Stripe's
	// create_invoice/refund_payment are the only handlers that actually
	// use it — see mcp.WithIdempotencyKey's doc comment): protects
	// against double-execution if this exact logical tool call is ever
	// retried (an approval batch re-Resume()'d, a crash-recovery
	// replay). {run_id}-{tool_call_index} is deterministic and stable —
	// ToolCallCount is the index, read before it's incremented below.
	var idempotencyKey string
	if RequiresApproval(deps.Tools.RiskLevel(tc.Name)) {
		idempotencyKey = fmt.Sprintf("%s-%d", state.WorkflowRunID, state.ToolCallCount)
	}

	result, execErr := gatewayCallWithRetry(ctx, deps.Gateway, deps.OrgID, service, toolName, args, idempotencyKey)
	if err := writeCostLedgerToolCall(ctx, deps, state, tc.Name, execErr == nil); err != nil {
		slog.Error("graph: write cost_ledger for tool call failed", "run_id", state.WorkflowRunID, "tool", tc.Name, "err", err)
	}

	var resultText string
	var isError bool
	var errString string
	if execErr != nil {
		isError = true
		errString = execErr.Error()
		resultText = errString
	} else {
		resultText, isError = toolResultText(result)
	}
	if err := writeAuditLog(ctx, deps, state, "tool.executed", service, isError); err != nil {
		slog.Error("graph: write audit_logs for tool call failed", "run_id", state.WorkflowRunID, "tool", tc.Name, "err", err)
	}

	// The result text (truncated for display, same limit as the
	// model-context copy below) was already computed above and stored in
	// state.ToolResults, but never actually shown to a human watching the
	// run — the SSE event used to carry only a bare success/fail flag.
	deps.Engine.Bus.Publish(Event{Type: EventToolResult, RunID: state.WorkflowRunID, Data: map[string]any{
		"tool": tc.Name, "is_error": isError, "result": truncateForContext(resultText),
	}})

	// Truncated copy for the model's own context — the full,
	// untruncated resultText still goes into ToolResults below
	state.Conversation = append(state.Conversation, llm.Message{
		Role: llm.RoleTool, Name: tc.Name, ToolCallID: tc.ID, Content: truncateForContext(resultText), IsError: isError,
	})
	state.ToolResults = append(state.ToolResults, ToolResult{
		ToolName: tc.Name, Args: tc.Args, Result: json.RawMessage(marshalOrQuote(resultText)), Error: errString,
	})
	state.ToolCallCount++

	if err := deps.Engine.Checkpoint(ctx, state, "executor"); err != nil {
		return fmt.Errorf("graph: checkpoint after tool call: %w", err)
	}
	return nil
}

// gatewayMaxAttempts bounds retry for a Gateway-level (not tool-handler-level)
// retryable failure — currently just a tripped rate limit.
const gatewayMaxAttempts = 3

// gatewayCallWithRetry retries Gateway.ExecuteTool only when it returns
// an error classified coremcp.ErrToolRetryable — currently only a
// tripped rate limit reaches this (a tool-handler error is already
// flattened into result.IsError by the time ExecuteTool returns, per
// mcp.ToolHandlerFor's own documented behavior, so it's never a Go
// error here). Any other error (unknown service, token fetch failure,
// ...) returns immediately, unretried.
func gatewayCallWithRetry(ctx context.Context, gateway *coremcp.Gateway, orgID pgtype.UUID, service, toolName string, args map[string]any, idempotencyKey string) (*gomcp.CallToolResult, error) {
	var lastErr error
	for attempt := 1; attempt <= gatewayMaxAttempts; attempt++ {
		result, err := gateway.ExecuteTool(ctx, orgID, service, toolName, args, idempotencyKey)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !errors.Is(err, coremcp.ErrToolRetryable) {
			return nil, err
		}
		if attempt < gatewayMaxAttempts {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
	}
	return nil, lastErr
}

// toolResultContextLimit caps how much of a tool result's text gets fed
// back into the model's own context — a large Stripe/Notion/GitHub
// payload shouldn't dominate every subsequent turn's token budget. The
// full, untruncated result is never affected — only the copy appended to
// state.Conversation goes through this; state.ToolResults (the
// audit-facing record) and the cost_ledger/audit_logs writes both still
// see the complete text.
const toolResultContextLimit = 4000

func truncateForContext(text string) string {
	if len(text) <= toolResultContextLimit {
		return text
	}
	return text[:toolResultContextLimit] + fmt.Sprintf("\n...(truncated, %d bytes total — refine your request with more specific filters to see more)", len(text))
}

// toolResultText extracts a plain-text representation of a tool call's
// result: StructuredContent (the typed Out value every tool handler in
// this codebase returns) when present, else concatenated text content
// blocks.
func toolResultText(result *gomcp.CallToolResult) (text string, isError bool) {
	if result == nil {
		return "", false
	}
	isError = result.IsError
	if result.StructuredContent != nil {
		if b, err := json.Marshal(result.StructuredContent); err == nil {
			return string(b), isError
		}
	}
	var sb strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(*gomcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), isError
}

// marshalOrQuote keeps ToolResult.Result valid JSON (json.RawMessage)
// even when resultText isn't itself JSON (e.g. a plain error string).
func marshalOrQuote(s string) []byte {
	if json.Valid([]byte(s)) {
		return []byte(s)
	}
	b, _ := json.Marshal(s)
	return b
}

// accumulateUsage folds one Send call's token usage into the run's
// running total, plus a rough dollar estimate (llm.EstimateCostUSD, keyed
// by model — see its doc comment for why this is an estimate, not
// billing-grade) into CostSoFarUSD, which policy_scope.max_cost_per_run_usd
// is actually enforced against.
func accumulateUsage(state *RunState, model string, u llm.TokenUsage) {
	state.TokenUsage.InputTokens += u.InputTokens
	state.TokenUsage.OutputTokens += u.OutputTokens
	state.TokenUsage.CachedTokens += u.CachedTokens
	state.CostSoFarUSD += llm.EstimateCostUSD(model, u)
}

// toolCallBatchSignature is order-independent within a batch (the model
// may reorder identical requests between turns without that being a
// meaningfully different batch for stuck-loop purposes).
func toolCallBatchSignature(calls []llm.ToolCall) string {
	parts := make([]string, len(calls))
	for i, c := range calls {
		parts[i] = c.Name + ":" + string(c.Args)
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

func toolCallNames(calls []llm.ToolCall) string {
	names := make([]string, len(calls))
	for i, c := range calls {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

// sendWithRetry bounds retries to llm.ErrChatUnavailable (transient —
// rate limit, network, 5xx) with a short linear backoff; ErrChatRejected
// and any other error are terminal and returned immediately.
func sendWithRetry(ctx context.Context, client llm.ChatClient, systemPrompt string, messages []llm.Message, tools []llm.ToolSchema) (llm.ChatResponse, error) {
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := client.Send(ctx, systemPrompt, messages, tools)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !errors.Is(err, llm.ErrChatUnavailable) {
			return llm.ChatResponse{}, err
		}
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return llm.ChatResponse{}, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
	}
	return llm.ChatResponse{}, fmt.Errorf("chat call failed after %d attempts: %w", maxAttempts, lastErr)
}

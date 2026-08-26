package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MockScenarioResolver is a dev-only ChatClientResolver (see
// graph.ChatClientResolver) that ignores the org's real BYOK key entirely
// and returns one of a fixed catalog of scripted MockChatClient scenarios,
// selected by the agent's plain `model` field — e.g. an agent configured
// with model "mock:tool-call" gets the tool-call-and-respond scenario.
// Wired in by cmd/api/main.go only when config.MockLLMMode is set (and
// only outside production — see Config.MockLLMMode's doc comment) so a
// founder without a real provider key yet can still exercise the whole
// workflow-9 harness (guardrails, approval suspend/resume, SSE events,
// checkpointing) against real Postgres through the real HTTP API, not
// just via the Go test suite. See WORKFLOW_PLAN_GO.md's Workflow 9
// manual-verification guide for the exact test-org setup and expected
// result per scenario.
//
// Unrecognized model strings are a hard error, not a silent fallback —
// this is a test tool; a typo should surface clearly (the run fails,
// findable in server logs) rather than quietly running the wrong
// scenario.
//
// "mock:approval" is handled separately from every other scenario below
// — see approvalScenarioClient's doc comment for a real bug this found:
// a plain sequential MockChatClient breaks across an approval-gate
// suspend/resume, because Resume triggers a brand new
// ChatClientResolver call (a fresh MockChatClient, canned-response index
// reset to 0), unlike a real provider client where "what comes next" is
// driven by the conversation history, not by how many times the client
// itself has been constructed.
func MockScenarioResolver(ctx context.Context, appPool *pgxpool.Pool, encryptionKey []byte, orgID pgtype.UUID, provider ProviderID, model string) (ChatClient, error) {
	if model == "mock:approval" {
		return &delayedChatClient{inner: approvalScenarioClient{}, delay: defaultMockDelay}, nil
	}
	scenario, ok := mockScenarios[model]
	if !ok {
		return nil, fmt.Errorf("llm: unknown mock scenario %q — valid scenarios: %s", model, mockScenarioNames())
	}
	client := NewMockChatClient(scenario.responses...)
	delay := scenario.delay
	if delay == 0 {
		delay = defaultMockDelay
	}
	return &delayedChatClient{inner: client, delay: delay}, nil
}

// approvalScenarioClient implements "mock:approval" without a sequential
// per-instance response counter (see MockScenarioResolver's doc comment
// for why one breaks here) — found by actually running this scenario
// end to end against a real Postgres-backed suspend/resume, not assumed
// correct from the code alone: the naive canned-sequence version aborted
// with a false "stuck loop detected" the instant a human approved,
// because the freshly-resolved MockChatClient's first response was once
// again the exact same stripe.refund_payment request the stuck-loop
// detector had already recorded as this run's last batch. Instead this
// decides its response from the actual conversation history Send is
// called with, the same way a real ChatClient effectively would: no
// stripe.refund_payment tool result present yet -> request it (first
// turn, pre-suspend); a tool result for it is already present -> the run
// is resuming post-approval, report done.
type approvalScenarioClient struct{}

func (approvalScenarioClient) Send(ctx context.Context, systemPrompt string, messages []Message, tools []ToolSchema) (ChatResponse, error) {
	for _, m := range messages {
		if m.Role == RoleTool && m.Name == "stripe.refund_payment" {
			return ChatResponse{StopReason: StopReasonEndTurn, Content: "Refund processed. Task complete.", Usage: turnUsage}, nil
		}
	}
	return ChatResponse{StopReason: StopReasonToolUse, Usage: turnUsage, ToolCalls: []ToolCall{
		{ID: "call_1", Name: "stripe.refund_payment", Args: toolCallArgs(`{"payment_intent_id":"pi_mock_123","amount_cents":500}`)},
	}}, nil
}

// defaultMockDelay is applied to every scenario that doesn't set its own
// (longer) delay. A MockChatClient otherwise returns in well under a
// millisecond — real enough to prove correctness against Postgres
// afterward, but too fast for a human watching GET /runs/{id}/stream live
// to see anything: by the time a manual `curl -N .../stream` request
// reaches the handler, an un-delayed mock run has usually already
// finished and the EventBus has nothing left to deliver (events aren't
// buffered/replayed for a subscriber that arrives late). A short, fixed
// delay per turn makes a manual SSE-watching session behave the way a
// real (much slower) LLM call naturally would.
const defaultMockDelay = 800 * time.Millisecond

type mockScenario struct {
	description string
	responses   []ChatResponse
	// delay overrides defaultMockDelay — only "mock:cancel" sets this
	// (much longer), giving a human enough wall-clock time to issue POST
	// /runs/{id}/cancel against a real, in-flight run before it finishes
	// on its own.
	delay time.Duration
}

func toolCallArgs(jsonArgs string) json.RawMessage { return json.RawMessage(jsonArgs) }

// turnUsage is a representative per-turn token count applied to every
// scenario response below — realistic enough that CostSoFarUSD actually
// accumulates something (see llm.EstimateCostUSD), which is what makes
// "mock:cost-cap" below able to exercise
// policy_scope.max_cost_per_run_usd at all. A MockChatClient response
// otherwise defaults its Usage to the zero value, which is why this
// guardrail looked untestable at first — cost never moved because these
// scenarios never reported any token usage, not just because
// RunState.CostSoFarUSD itself wasn't wired up (that was the other half
// of the same bug — see accumulateUsage's doc comment).
var turnUsage = TokenUsage{InputTokens: 500, OutputTokens: 150}

var mockScenarios = map[string]mockScenario{
	"mock:happy": {
		description: "single turn, no tool calls — proves node sequencing (planner→executor→validator→reporter) and the complete SSE event end to end",
		responses: []ChatResponse{
			{StopReason: StopReasonEndTurn, Content: "Task complete — no tools were needed for this run.", Usage: turnUsage},
		},
	},
	"mock:tool-call": {
		description: "requests notion.read_page once, reacts to whatever comes back (a real result if Notion is connected for this org, a terminal tool error otherwise) — proves tool_call/tool_result SSE events, per-tool-call checkpointing, and the cost_ledger/audit_logs writes",
		responses: []ChatResponse{
			{StopReason: StopReasonToolUse, Usage: turnUsage, ToolCalls: []ToolCall{
				{ID: "call_1", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-test-page"}`)},
			}},
			{StopReason: StopReasonEndTurn, Content: "Finished reviewing the page.", Usage: turnUsage},
		},
	},
	// "mock:approval" is NOT in this map — see MockScenarioResolver's and
	// approvalScenarioClient's doc comments for why it needs
	// conversation-aware logic instead of a plain canned sequence.
	"mock:policy-violation": {
		description: "requests notion.write_page — configure the test agent's allowed_tools to exclude it, so CheckToolAllowed rejects the call and the run aborts",
		responses: []ChatResponse{
			{StopReason: StopReasonToolUse, Usage: turnUsage, ToolCalls: []ToolCall{
				{ID: "call_1", Name: "notion.write_page", Args: toolCallArgs(`{"parent_page_id":"mock-parent","title":"Mock page","content":"hello"}`)},
			}},
		},
	},
	"mock:stuck-loop": {
		description: "requests the exact same notion.read_page call twice in a row — the stuck-loop detector aborts on the repeat",
		responses: []ChatResponse{
			{StopReason: StopReasonToolUse, Usage: turnUsage, ToolCalls: []ToolCall{
				{ID: "call_1", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-test-page"}`)},
			}},
			{StopReason: StopReasonToolUse, Usage: turnUsage, ToolCalls: []ToolCall{
				{ID: "call_2", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-test-page"}`)},
			}},
		},
	},
	"mock:tool-call-cap": {
		description: "requests notion.read_page 5 times in a row, each with a different page_id (so the stuck-loop detector never trips) — configure the test agent's policy_scope.max_tool_calls below 5 so CheckCaps aborts the run first",
		responses: []ChatResponse{
			{StopReason: StopReasonToolUse, Usage: turnUsage, ToolCalls: []ToolCall{{ID: "call_1", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-page-1"}`)}}},
			{StopReason: StopReasonToolUse, Usage: turnUsage, ToolCalls: []ToolCall{{ID: "call_2", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-page-2"}`)}}},
			{StopReason: StopReasonToolUse, Usage: turnUsage, ToolCalls: []ToolCall{{ID: "call_3", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-page-3"}`)}}},
			{StopReason: StopReasonToolUse, Usage: turnUsage, ToolCalls: []ToolCall{{ID: "call_4", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-page-4"}`)}}},
			{StopReason: StopReasonToolUse, Usage: turnUsage, ToolCalls: []ToolCall{{ID: "call_5", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-page-5"}`)}}},
		},
	},
	"mock:cost-cap": {
		description: "same 5-distinct-calls shape as mock:tool-call-cap, but each turn reports 5000 input / 2000 output tokens — configure the test agent's policy_scope.max_cost_per_run_usd low enough (e.g. 0.05) that CheckCaps' cost check aborts the run before max_tool_calls would",
		responses: []ChatResponse{
			{StopReason: StopReasonToolUse, Usage: TokenUsage{InputTokens: 5000, OutputTokens: 2000}, ToolCalls: []ToolCall{{ID: "call_1", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-page-1"}`)}}},
			{StopReason: StopReasonToolUse, Usage: TokenUsage{InputTokens: 5000, OutputTokens: 2000}, ToolCalls: []ToolCall{{ID: "call_2", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-page-2"}`)}}},
			{StopReason: StopReasonToolUse, Usage: TokenUsage{InputTokens: 5000, OutputTokens: 2000}, ToolCalls: []ToolCall{{ID: "call_3", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-page-3"}`)}}},
			{StopReason: StopReasonToolUse, Usage: TokenUsage{InputTokens: 5000, OutputTokens: 2000}, ToolCalls: []ToolCall{{ID: "call_4", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-page-4"}`)}}},
			{StopReason: StopReasonToolUse, Usage: TokenUsage{InputTokens: 5000, OutputTokens: 2000}, ToolCalls: []ToolCall{{ID: "call_5", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-page-5"}`)}}},
		},
	},
	"mock:cancel": {
		description: "same shape as mock:tool-call-cap (5 distinct tool-call turns) but each Send call sleeps 5s first — gives a human time to POST /runs/{id}/cancel against a real in-flight run",
		delay:       5 * time.Second,
		responses: []ChatResponse{
			{StopReason: StopReasonToolUse, Usage: turnUsage, ToolCalls: []ToolCall{{ID: "call_1", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-page-1"}`)}}},
			{StopReason: StopReasonToolUse, Usage: turnUsage, ToolCalls: []ToolCall{{ID: "call_2", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-page-2"}`)}}},
			{StopReason: StopReasonToolUse, Usage: turnUsage, ToolCalls: []ToolCall{{ID: "call_3", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-page-3"}`)}}},
			{StopReason: StopReasonToolUse, Usage: turnUsage, ToolCalls: []ToolCall{{ID: "call_4", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-page-4"}`)}}},
			{StopReason: StopReasonToolUse, Usage: turnUsage, ToolCalls: []ToolCall{{ID: "call_5", Name: "notion.read_page", Args: toolCallArgs(`{"page_id":"mock-page-5"}`)}}},
		},
	},
}

func mockScenarioNames() string {
	names := make([]string, 0, len(mockScenarios)+1)
	names = append(names, "mock:approval") // handled outside mockScenarios — see MockScenarioResolver
	for name := range mockScenarios {
		names = append(names, name)
	}
	return fmt.Sprintf("%v", names)
}

// delayedChatClient wraps a ChatClient with a fixed delay before each
// Send call, honoring ctx cancellation while waiting rather than blocking
// past it — needed so "mock:cancel" gives a human real wall-clock time to
// cancel an in-flight run without also breaking Engine.Cancel's own
// ctx-propagation guarantee.
type delayedChatClient struct {
	inner ChatClient
	delay time.Duration
}

func (d *delayedChatClient) Send(ctx context.Context, systemPrompt string, messages []Message, tools []ToolSchema) (ChatResponse, error) {
	select {
	case <-ctx.Done():
		return ChatResponse{}, ctx.Err()
	case <-time.After(d.delay):
	}
	return d.inner.Send(ctx, systemPrompt, messages, tools)
}

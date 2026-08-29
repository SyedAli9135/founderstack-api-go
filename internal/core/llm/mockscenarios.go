package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/core/integrations"
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
		client := approvalScenarioClient{appPool: appPool, encryptionKey: encryptionKey, orgID: orgID}
		return &delayedChatClient{inner: client, delay: defaultMockDelay}, nil
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
// appPool/encryptionKey/orgID let Send mint a real, fresh Stripe
// test-mode PaymentIntent on the pre-suspend turn (see below) — added so
// this scenario's refund_payment call genuinely succeeds against a real
// account instead of a hardcoded placeholder ID, which could only ever
// succeed once (a real PaymentIntent can only be fully refunded once).
type approvalScenarioClient struct {
	appPool       *pgxpool.Pool
	encryptionKey []byte
	orgID         pgtype.UUID
}

func (c approvalScenarioClient) Send(ctx context.Context, systemPrompt string, messages []Message, tools []ToolSchema) (ChatResponse, error) {
	for _, m := range messages {
		if m.Role == RoleTool && m.Name == "stripe.refund_payment" {
			return ChatResponse{
				StopReason: StopReasonEndTurn, Usage: turnUsage,
				Content: "The refund went through — I can see the payment intent now shows a succeeded refund. Task complete.",
			}, nil
		}
	}

	// This is the pre-suspend turn — mint a fresh, real Stripe test-mode
	// customer + confirmed PaymentIntent (test card, zero real money) so
	// the refund_payment call below hits a genuinely refundable target
	// every time this scenario runs, not a hardcoded ID that goes stale
	// after its first successful refund. Only called here, once, not on
	// every Resume — Resume re-resolves a fresh approvalScenarioClient too
	// (see MockScenarioResolver's doc comment), but by then a tool result
	// is already present above, so this branch never runs twice per run.
	// Falls back to a placeholder ID (a real, useful tool-error demo, not
	// a broken run) if this org has no working Stripe connection at all.
	paymentIntentID, err := createTestPaymentIntent(ctx, c.appPool, c.encryptionKey, c.orgID)
	if err != nil {
		paymentIntentID = "pi_mock_no_real_stripe_connection"
	}

	return ChatResponse{
		StopReason: StopReasonToolUse, Usage: turnUsage,
		Content: "This customer is requesting a $5.00 refund on their recent payment. Refunds always need a human's sign-off before I execute them, so I'll request approval before calling the refund tool.",
		ToolCalls: []ToolCall{
			{ID: "call_1", Name: "stripe.refund_payment", Args: toolCallArgs(fmt.Sprintf(
				`{"payment_intent_id":%q,"amount_cents":500}`, paymentIntentID))},
		},
	}, nil
}

// createTestPaymentIntent creates a real Stripe test-mode customer plus a
// confirmed PaymentIntent (pm_card_visa, Stripe's standard always-succeeds
// test card — no real money moves in test mode) using the org's own
// stored Stripe token. Test-fixture setup only, not a tool call itself —
// mirrors the exact recipe manually verified live 2026-08-28.
func createTestPaymentIntent(ctx context.Context, appPool *pgxpool.Pool, encryptionKey []byte, orgID pgtype.UUID) (string, error) {
	tok, err := integrations.GetIntegrationToken(ctx, appPool, encryptionKey, orgID, "stripe")
	if err != nil {
		return "", fmt.Errorf("llm: fetch stripe token for mock:approval fixture: %w", err)
	}
	customerID, err := stripeFixturePost(ctx, tok.AccessToken, "https://api.stripe.com/v1/customers", url.Values{
		"description": {"FounderStack mock:approval scenario fixture"},
	})
	if err != nil {
		return "", err
	}
	return stripeFixturePost(ctx, tok.AccessToken, "https://api.stripe.com/v1/payment_intents", url.Values{
		"amount":                             {"500"},
		"currency":                           {"usd"},
		"customer":                           {customerID},
		"payment_method":                     {"pm_card_visa"},
		"confirm":                            {"true"},
		"automatic_payment_methods[enabled]": {"true"},
		"automatic_payment_methods[allow_redirects]": {"never"},
	})
}

func stripeFixturePost(ctx context.Context, secretKey, endpoint string, form url.Values) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(secretKey, "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("llm: stripe fixture call %s failed (%d): %s", endpoint, resp.StatusCode, body)
	}
	var decoded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", err
	}
	if decoded.ID == "" {
		return "", fmt.Errorf("llm: stripe fixture response from %s missing id: %s", endpoint, body)
	}
	return decoded.ID, nil
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

// mockNotionPages are 5 real, permanent Notion pages (created 2026-08-28
// specifically for this purpose — "[FounderStack Mock] Reference Page N",
// children of the founder's own real, already-shared "SQL Training Plan"
// page) that every multi-call scenario below reads from. Using real page
// IDs — instead of the placeholder "mock-page-N" strings the original
// build used — means these scenarios' tool calls genuinely succeed
// against the real Notion API, not just correctly fail. 5 *distinct*
// pages are needed (not 1 reused 5 times): toolCallBatchSignature keys
// off a call's exact args, so 5 identical page_ids would falsely trip the
// stuck-loop detector instead of exercising the cap/cost/cancel guardrail
// each of these scenarios actually means to test.
var mockNotionPages = [5]string{
	"2cfa27a8-4499-80a3-94e4-e9cc897c1297", // SQL Training Plan
	"3caa27a8-4499-811e-b48a-d987792b5f65", // Reference Page 1
	"3caa27a8-4499-815b-9e3c-e7e721f3a6d8", // Reference Page 2
	"3caa27a8-4499-81ed-ac78-f795a2ee0296", // Reference Page 3
	"3caa27a8-4499-8101-a279-e44860b26d08", // Reference Page 4
}

// mockReadPageCall builds one notion.read_page turn against
// mockNotionPages[i], with reasoning text a real model would plausibly
// produce right before making that call — see executorNode's new
// EventReasoning publish, which surfaces ChatResponse.Content live even
// on a turn that also requests a tool call (confirmed this is exactly
// what real providers like Anthropic do, not just a final-answer-only
// field).
func mockReadPageCall(i int, callID, reasoning string) ChatResponse {
	return ChatResponse{
		StopReason: StopReasonToolUse, Usage: turnUsage, Content: reasoning,
		ToolCalls: []ToolCall{{ID: callID, Name: "notion.read_page", Args: toolCallArgs(
			fmt.Sprintf(`{"page_id":%q}`, mockNotionPages[i]))}},
	}
}

var mockScenarios = map[string]mockScenario{
	"mock:happy": {
		description: "single turn, no tool calls — proves node sequencing (planner→executor→validator→reporter) and the complete SSE event end to end",
		responses: []ChatResponse{
			{StopReason: StopReasonEndTurn, Content: "This task doesn't require any tools — I can answer directly. Task complete.", Usage: turnUsage},
		},
	},
	"mock:tool-call": {
		description: "requests notion.read_page once against a real, permanently-shared Notion page — proves tool_call/tool_result SSE events (including real result content), per-tool-call checkpointing, and the cost_ledger/audit_logs writes",
		responses: []ChatResponse{
			mockReadPageCall(0, "call_1", "I'll pull up the referenced Notion page to review its contents before responding."),
			{StopReason: StopReasonEndTurn, Content: "I've reviewed the page — it's the SQL training plan. Finished reviewing the page.", Usage: turnUsage},
		},
	},
	// "mock:approval" is NOT in this map — see MockScenarioResolver's and
	// approvalScenarioClient's doc comments for why it needs
	// conversation-aware logic instead of a plain canned sequence.
	"mock:policy-violation": {
		description: "requests notion.write_page — configure the test agent's allowed_tools to exclude it, so CheckToolAllowed rejects the call and the run aborts",
		responses: []ChatResponse{
			{
				StopReason: StopReasonToolUse, Usage: turnUsage,
				Content: "I'll create a new sub-page summarizing this content under the reference page.",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "notion.write_page", Args: toolCallArgs(fmt.Sprintf(
					`{"parent_page_id":%q,"title":"Mock summary page","body":"This call should never actually reach Notion — it's blocked by policy_scope before dispatch."}`,
					mockNotionPages[0]))}},
			},
		},
	},
	"mock:stuck-loop": {
		description: "requests the exact same notion.read_page call twice in a row — the stuck-loop detector aborts on the repeat",
		responses: []ChatResponse{
			mockReadPageCall(0, "call_1", "Let me check this page for the information I need."),
			mockReadPageCall(0, "call_2", "I still need to check this page for the information I need."),
		},
	},
	"mock:tool-call-cap": {
		description: "requests notion.read_page 5 times in a row against 5 distinct real pages — configure the test agent's policy_scope.max_tool_calls below 5 so CheckCaps aborts the run first",
		responses: []ChatResponse{
			mockReadPageCall(0, "call_1", "Let's start by reviewing the SQL training plan page."),
			mockReadPageCall(1, "call_2", "Now checking the first reference page for related context."),
			mockReadPageCall(2, "call_3", "Continuing on to the second reference page."),
			mockReadPageCall(3, "call_4", "One more — the third reference page."),
			mockReadPageCall(4, "call_5", "And the fourth reference page, to be thorough."),
		},
	},
	"mock:cost-cap": {
		description: "same 5-distinct-real-pages shape as mock:tool-call-cap, but each turn reports 5000 input / 2000 output tokens — configure the test agent's policy_scope.max_cost_per_run_usd low enough (e.g. 0.05) that CheckCaps' cost check aborts the run before max_tool_calls would",
		responses: []ChatResponse{
			{StopReason: StopReasonToolUse, Usage: TokenUsage{InputTokens: 5000, OutputTokens: 2000}, Content: "Let's start by reviewing the SQL training plan page.", ToolCalls: mockReadPageCall(0, "call_1", "").ToolCalls},
			{StopReason: StopReasonToolUse, Usage: TokenUsage{InputTokens: 5000, OutputTokens: 2000}, Content: "Now checking the first reference page for related context.", ToolCalls: mockReadPageCall(1, "call_2", "").ToolCalls},
			{StopReason: StopReasonToolUse, Usage: TokenUsage{InputTokens: 5000, OutputTokens: 2000}, Content: "Continuing on to the second reference page.", ToolCalls: mockReadPageCall(2, "call_3", "").ToolCalls},
			{StopReason: StopReasonToolUse, Usage: TokenUsage{InputTokens: 5000, OutputTokens: 2000}, Content: "One more — the third reference page.", ToolCalls: mockReadPageCall(3, "call_4", "").ToolCalls},
			{StopReason: StopReasonToolUse, Usage: TokenUsage{InputTokens: 5000, OutputTokens: 2000}, Content: "And the fourth reference page, to be thorough.", ToolCalls: mockReadPageCall(4, "call_5", "").ToolCalls},
		},
	},
	// The 5 scenarios below (mock:slack/discord/drive/calendar/github) each
	// exercise one more of the 8 connected tool servers with the same
	// real-resource-plus-reasoning treatment as mock:tool-call above —
	// added 2026-08-28 alongside it, once the founder asked to see the
	// same real-execution proof for every integration, not just Notion
	// and Stripe. Each points at a small, stable, real resource created
	// specifically for this (a Drive file, a Calendar event, a GitHub PR)
	// so these scenarios keep working indefinitely without depending on
	// data that might get cleaned up elsewhere.
	"mock:slack": {
		description: "requests slack.list_channels once against the real connected workspace — read-only, so safe to re-run indefinitely",
		responses: []ChatResponse{
			{
				StopReason: StopReasonToolUse, Usage: turnUsage,
				Content:   "Let me check which Slack channels I have access to before posting anything.",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "slack.list_channels", Args: toolCallArgs(`{"limit":20}`)}},
			},
			{StopReason: StopReasonEndTurn, Content: "I can see the team's channels now. Finished checking Slack.", Usage: turnUsage},
		},
	},
	"mock:discord": {
		description: "requests discord.send_message once against the real connected webhook — the only tool this server has, so this genuinely posts a real message every run (see NewDiscordServer's doc comment for why there's no read-only alternative)",
		responses: []ChatResponse{
			{
				StopReason: StopReasonToolUse, Usage: turnUsage,
				Content: "I'll post a status update to the team's Discord channel.",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "discord.send_message", Args: toolCallArgs(
					`{"content":"[FounderStack Mock] Automated status update from the agent harness — this is a scripted test message."}`)}},
			},
			{StopReason: StopReasonEndTurn, Content: "Message posted successfully. Finished the Discord update.", Usage: turnUsage},
		},
	},
	"mock:drive": {
		description: "requests google_drive.read_file once against a stable real reference file created for this scenario — read-only, safe to re-run indefinitely",
		responses: []ChatResponse{
			{
				StopReason: StopReasonToolUse, Usage: turnUsage,
				Content: "Let me check this reference file's contents before responding.",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "google_drive.read_file", Args: toolCallArgs(
					`{"file_id":"1qdk7ueHBzrYe3X2s_7RyYpBeI7CWE-D0"}`)}},
			},
			{StopReason: StopReasonEndTurn, Content: "Reviewed the file — it's the mock testing reference doc. Finished reviewing.", Usage: turnUsage},
		},
	},
	"mock:calendar": {
		description: "requests google_calendar.list_events once, windowed around a stable real reference event created for this scenario — read-only, safe to re-run indefinitely",
		responses: []ChatResponse{
			{
				StopReason: StopReasonToolUse, Usage: turnUsage,
				Content: "Let me check what's on the calendar for that period.",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "google_calendar.list_events", Args: toolCallArgs(
					`{"time_min":"2027-01-01T00:00:00Z","time_max":"2027-01-02T00:00:00Z"}`)}},
			},
			{StopReason: StopReasonEndTurn, Content: "Found the reference event on the calendar. Finished checking.", Usage: turnUsage},
		},
	},
	"mock:github": {
		description: "requests github.review_pr once against a stable, permanently-open reference PR created for this scenario — read-only, safe to re-run indefinitely",
		responses: []ChatResponse{
			{
				StopReason: StopReasonToolUse, Usage: turnUsage,
				Content: "Let me review this pull request's changes before responding.",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "github.review_pr", Args: toolCallArgs(
					`{"owner":"SyedAli9135","repo":"founderstack-api-go","number":3}`)}},
			},
			{StopReason: StopReasonEndTurn, Content: "Reviewed the PR — it's just the mock testing reference file. Finished the review.", Usage: turnUsage},
		},
	},
	// The 10 scenarios below round out every remaining tool across all 8
	// connected servers (LinkedIn's draft_post is the one deliberate
	// exception — a real public/permanent post, left to unit-test-only
	// coverage per the founder's own call) — added 2026-08-28 once the
	// founder pointed out most services only had one of their several
	// tools actually exercised by a mock scenario. Same real-resource,
	// real-reasoning treatment as everything above.
	"mock:notion-write": {
		description: "requests notion.write_page against the real, already-shared parent page — creates a genuinely new real page every run (repeatable write, like mock:discord)",
		responses: []ChatResponse{
			{
				StopReason: StopReasonToolUse, Usage: turnUsage,
				Content: "I'll create a new sub-page summarizing this under the reference page.",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "notion.write_page", Args: toolCallArgs(fmt.Sprintf(
					`{"parent_page_id":%q,"title":"[FounderStack Mock] write_page scenario output","body":"This page was created by the mock:notion-write scenario — a genuine notion.write_page call, not a placeholder. Safe to delete."}`,
					mockNotionPages[0]))}},
			},
			{StopReason: StopReasonEndTurn, Content: "Created the summary page. Finished.", Usage: turnUsage},
		},
	},
	"mock:stripe-list": {
		description: "requests stripe.list_subscriptions — read-only, safe to re-run indefinitely",
		responses: []ChatResponse{
			{StopReason: StopReasonToolUse, Usage: turnUsage, Content: "Let me check this account's active subscriptions.",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "stripe.list_subscriptions", Args: toolCallArgs(`{"limit":20}`)}}},
			{StopReason: StopReasonEndTurn, Content: "Checked the subscriptions. Finished.", Usage: turnUsage},
		},
	},
	"mock:stripe-invoice": {
		description: "requests stripe.create_invoice against a stable real test-mode customer — creates a genuinely new real draft invoice every run (repeatable write)",
		responses: []ChatResponse{
			{StopReason: StopReasonToolUse, Usage: turnUsage, Content: "I'll draft an invoice for this customer.",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "stripe.create_invoice", Args: toolCallArgs(
					`{"customer_id":"cus_V9dWbpuHoWvQ70","amount_cents":500,"description":"mock:stripe-invoice scenario output"}`)}}},
			{StopReason: StopReasonEndTurn, Content: "Drafted the invoice. Finished.", Usage: turnUsage},
		},
	},
	"mock:stripe-mrr": {
		description: "requests stripe.get_mrr — read-only, safe to re-run indefinitely",
		responses: []ChatResponse{
			{StopReason: StopReasonToolUse, Usage: turnUsage, Content: "Let me pull the current MRR estimate.",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "stripe.get_mrr", Args: toolCallArgs(`{}`)}}},
			{StopReason: StopReasonEndTurn, Content: "Got the MRR estimate. Finished.", Usage: turnUsage},
		},
	},
	"mock:github-issue": {
		description: "requests github.create_issue against the real repo — creates a genuinely new real issue every run (repeatable write, like mock:discord)",
		responses: []ChatResponse{
			{StopReason: StopReasonToolUse, Usage: turnUsage, Content: "I'll file an issue documenting this.",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "github.create_issue", Args: toolCallArgs(
					`{"owner":"SyedAli9135","repo":"founderstack-api-go","title":"[FounderStack Mock] create_issue scenario output","body":"Filed by the mock:github-issue scenario — a genuine github.create_issue call. Safe to close."}`)}}},
			{StopReason: StopReasonEndTurn, Content: "Filed the issue. Finished.", Usage: turnUsage},
		},
	},
	"mock:github-search": {
		description: "requests github.search_code against a large, well-indexed public repo (golang/go — the founder's own small repo isn't indexed by GitHub's search yet, a known platform limitation, not a bug) — read-only, safe to re-run indefinitely",
		responses: []ChatResponse{
			{StopReason: StopReasonToolUse, Usage: turnUsage, Content: "Let me search for a relevant code reference.",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "github.search_code", Args: toolCallArgs(
					`{"query":"repo:golang/go func main language:go","limit":5}`)}}},
			{StopReason: StopReasonEndTurn, Content: "Found relevant results. Finished.", Usage: turnUsage},
		},
	},
	"mock:drive-list": {
		description: "requests google_drive.list_files — read-only, safe to re-run indefinitely (will see at least the stable reference file created for mock:drive)",
		responses: []ChatResponse{
			{StopReason: StopReasonToolUse, Usage: turnUsage, Content: "Let me see what files this app has access to.",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "google_drive.list_files", Args: toolCallArgs(`{"limit":20}`)}}},
			{StopReason: StopReasonEndTurn, Content: "Checked the files. Finished.", Usage: turnUsage},
		},
	},
	"mock:drive-create": {
		description: "requests google_drive.create_file — creates a genuinely new real file every run (repeatable write, like mock:discord)",
		responses: []ChatResponse{
			{StopReason: StopReasonToolUse, Usage: turnUsage, Content: "I'll save a summary file for this.",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "google_drive.create_file", Args: toolCallArgs(
					`{"name":"[FounderStack Mock] create_file scenario output.txt","content":"Created by the mock:drive-create scenario — a genuine google_drive.create_file call. Safe to delete.","mime_type":"text/plain"}`)}}},
			{StopReason: StopReasonEndTurn, Content: "Saved the file. Finished.", Usage: turnUsage},
		},
	},
	"mock:calendar-create": {
		description: "requests google_calendar.create_event — creates a genuinely new real event every run (repeatable write, like mock:discord)",
		responses: []ChatResponse{
			{StopReason: StopReasonToolUse, Usage: turnUsage, Content: "I'll schedule a follow-up for this.",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "google_calendar.create_event", Args: toolCallArgs(
					`{"summary":"[FounderStack Mock] create_event scenario output","description":"Created by the mock:calendar-create scenario. Safe to delete.","start_time":"2027-06-01T10:00:00Z","end_time":"2027-06-01T10:30:00Z"}`)}}},
			{StopReason: StopReasonEndTurn, Content: "Scheduled the event. Finished.", Usage: turnUsage},
		},
	},
	"mock:slack-send": {
		description: "requests slack.send_message against the real connected workspace (#general, where the bot has already been invited) — creates a genuinely new real message every run (repeatable write, like mock:discord)",
		responses: []ChatResponse{
			{StopReason: StopReasonToolUse, Usage: turnUsage, Content: "I'll post a status update to the team's Slack channel.",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "slack.send_message", Args: toolCallArgs(
					`{"channel":"C0690RR4D3R","text":"[FounderStack Mock] Automated status update from the mock:slack-send scenario."}`)}}},
			{StopReason: StopReasonEndTurn, Content: "Posted the update. Finished.", Usage: turnUsage},
		},
	},
	"mock:cancel": {
		description: "same shape as mock:tool-call-cap (5 distinct tool-call turns against 5 distinct real pages) but each Send call sleeps 5s first — gives a human time to POST /runs/{id}/cancel against a real in-flight run",
		delay:       5 * time.Second,
		responses: []ChatResponse{
			mockReadPageCall(0, "call_1", "Let's start by reviewing the SQL training plan page."),
			mockReadPageCall(1, "call_2", "Now checking the first reference page for related context."),
			mockReadPageCall(2, "call_3", "Continuing on to the second reference page."),
			mockReadPageCall(3, "call_4", "One more — the third reference page."),
			mockReadPageCall(4, "call_5", "And the fourth reference page, to be thorough."),
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

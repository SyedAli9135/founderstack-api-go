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

// MockScenarioResolver is a dev-only ChatClientResolver that ignores the
// org's real BYOK key and returns one of a fixed catalog of scripted
// MockChatClient scenarios, selected by the agent's plain `model` field.
// Wired in only when config.MockLLMMode is set (never in production) so
// the whole workflow-9 harness is exercisable through the real HTTP API
// without a live provider key. Unrecognized model strings are a hard
// error, not a silent fallback — a typo should fail loudly.
//
// "mock:approval" is handled separately — see approvalScenarioClient: a
// plain sequential MockChatClient breaks across an approval-gate
// suspend/resume, since Resume triggers a fresh ChatClientResolver call
// (canned-response index reset to 0) unlike a real provider, where "what
// comes next" is driven by conversation history, not construction count.
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

// approvalScenarioClient implements "mock:approval" by deciding its
// response from conversation history instead of a sequential counter (see
// MockScenarioResolver): no stripe.refund_payment result yet -> request it
// (pre-suspend); a result is already present -> resuming post-approval,
// report done. appPool/encryptionKey/orgID let Send mint a real, fresh
// Stripe test-mode PaymentIntent on the pre-suspend turn, since a
// hardcoded ID can only ever be refunded once.
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

	// Pre-suspend turn: mint a fresh, real Stripe test-mode PaymentIntent
	// so refund_payment hits a genuinely refundable target every run.
	// Falls back to a placeholder ID (a useful tool-error demo, not a
	// broken run) if this org has no working Stripe connection.
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
// confirmed PaymentIntent (pm_card_visa, Stripe's always-succeeds test
// card) using the org's own stored Stripe token. Fixture setup only, not
// a tool call itself.
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

// defaultMockDelay applies to any scenario without its own (longer) delay.
// A MockChatClient otherwise returns in under a millisecond — too fast for
// a human watching GET /runs/{id}/stream to see anything, since events
// aren't buffered/replayed for a subscriber that arrives late.
const defaultMockDelay = 800 * time.Millisecond

type mockScenario struct {
	description string
	responses   []ChatResponse
	// delay overrides defaultMockDelay — only "mock:cancel" sets this, to
	// give a human time to POST /runs/{id}/cancel before the run finishes.
	delay time.Duration
}

func toolCallArgs(jsonArgs string) json.RawMessage { return json.RawMessage(jsonArgs) }

// turnUsage is a representative per-turn token count applied to every
// scenario response below, so CostSoFarUSD actually accumulates something
// (a bare MockChatClient response otherwise defaults Usage to zero) —
// needed for "mock:cost-cap" to exercise max_cost_per_run_usd at all.
var turnUsage = TokenUsage{InputTokens: 500, OutputTokens: 150}

// mockNotionPages are 5 real, permanent Notion pages that every
// multi-call scenario below reads from — real IDs so these tool calls
// genuinely succeed, not just correctly fail. 5 *distinct* pages are
// needed: toolCallBatchSignature keys off a call's exact args, so 5
// identical page_ids would falsely trip the stuck-loop detector instead
// of exercising the cap/cost/cancel guardrail each scenario means to test.
var mockNotionPages = [5]string{
	"2cfa27a8-4499-80a3-94e4-e9cc897c1297", // SQL Training Plan
	"3caa27a8-4499-811e-b48a-d987792b5f65", // Reference Page 1
	"3caa27a8-4499-815b-9e3c-e7e721f3a6d8", // Reference Page 2
	"3caa27a8-4499-81ed-ac78-f795a2ee0296", // Reference Page 3
	"3caa27a8-4499-8101-a279-e44860b26d08", // Reference Page 4
}

// mockReadPageCall builds one notion.read_page turn with reasoning text a
// real model would plausibly produce before the call — exercises
// executorNode's EventReasoning publish, which surfaces ChatResponse.Content
// even on a turn that also requests a tool call.
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
	// "mock:approval" is NOT in this map — see approvalScenarioClient.
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
	// The 5 scenarios below each exercise one more of the 8 connected tool
	// servers, same real-resource-plus-reasoning treatment as mock:tool-call.
	// Each points at a small, stable, real resource (a Drive file, a
	// Calendar event, a GitHub PR) so they keep working indefinitely.
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
	// connected servers (LinkedIn's draft_post is the one exception — a
	// real public/permanent post, left to unit-test-only coverage). Same
	// real-resource, real-reasoning treatment as everything above.
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

// delayedChatClient wraps a ChatClient with a fixed delay before each Send
// call, honoring ctx cancellation while waiting rather than blocking past
// it — preserves Engine.Cancel's ctx-propagation guarantee.
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

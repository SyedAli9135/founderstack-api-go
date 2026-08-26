package llm

import "strings"

// EstimateCostUSD gives a rough per-call dollar estimate for one LLM
// Send call's token usage, keyed by loose substring matches against the
// agent's configured model string. This is deliberately NOT billing-grade
// — provider prices change over time and this table isn't kept in sync
// with them automatically — same "estimate for planning purposes, not a
// billing-grade reconciliation figure" framing this codebase already uses
// for Stripe's get_mrr tool. It exists so
// agents.policy_scope.max_cost_per_run_usd has something real to check
// against (previously RunState.CostSoFarUSD was never incremented at
// all, so that guardrail could never actually trip — see
// WORKFLOW_PLAN_GO.md's Workflow 9 harness notes and
// MOCK_LLM_TESTING.md's "Known, deliberate gaps" section for how this was
// found). Real per-token billing-grade cost accounting is workflow 11's
// job — this is the harness's own interim guardrail-enforcement number,
// not what a founder's "you paid $X" summary should ultimately be
// computed from.
//
// Rates are per 1,000 tokens, roughly reflecting representative pricing
// as of when this table was written (2026-08-26) — periodically worth a
// manual refresh, not meant to track live pricing pages.
func EstimateCostUSD(model string, usage TokenUsage) float64 {
	rate := ratesFor(model)
	// Cached input tokens are billed at a steep discount by every
	// provider here (roughly a 90% discount on Anthropic's prompt
	// caching, the closest real reference point) — applied uniformly
	// rather than per-provider, since this is already an estimate.
	const cachedDiscount = 0.1
	cost := float64(usage.InputTokens)/1000*rate.inputPer1K +
		float64(usage.CachedTokens)/1000*rate.inputPer1K*cachedDiscount +
		float64(usage.OutputTokens)/1000*rate.outputPer1K
	return cost
}

type tokenRate struct {
	inputPer1K  float64
	outputPer1K float64
}

// fallbackRate covers anything not matched below — including every
// MOCK_LLM_MODE scenario's "mock:..." model string, which is deliberate:
// cost still accumulates in mock mode, so mock:tool-call-cap-style
// scenarios can exercise the cost cap the same way they exercise the
// tool-call cap, without needing a real provider's pricing to be modeled.
var fallbackRate = tokenRate{inputPer1K: 0.001, outputPer1K: 0.003}

var modelRates = []struct {
	substr string
	rate   tokenRate
}{
	{"claude", tokenRate{inputPer1K: 0.003, outputPer1K: 0.015}},
	{"gpt", tokenRate{inputPer1K: 0.0025, outputPer1K: 0.01}},
	{"gemini", tokenRate{inputPer1K: 0.00125, outputPer1K: 0.005}},
	{"qwen", tokenRate{inputPer1K: 0.0005, outputPer1K: 0.0015}},
	{"deepseek", tokenRate{inputPer1K: 0.0005, outputPer1K: 0.0015}},
}

func ratesFor(model string) tokenRate {
	lower := strings.ToLower(model)
	for _, m := range modelRates {
		if strings.Contains(lower, m.substr) {
			return m.rate
		}
	}
	return fallbackRate
}

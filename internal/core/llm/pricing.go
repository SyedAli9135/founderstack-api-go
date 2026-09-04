package llm

import "strings"

// EstimateCostUSD gives a rough per-call dollar estimate, keyed by loose
// substring match against the agent's model string. Deliberately NOT
// billing-grade (real per-token accounting is workflow 11's job) — this
// only exists so policy_scope.max_cost_per_run_usd has a real number to
// check against. Rates are per 1,000 tokens, a periodic-refresh snapshot
// rather than live pricing.
func EstimateCostUSD(model string, usage TokenUsage) float64 {
	rate := ratesFor(model)
	// ~90% discount, matching Anthropic's prompt-caching rate — applied
	// uniformly since this is already an estimate.
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

// fallbackRate also covers every "mock:..." model string — cost still
// accumulates in mock mode so cost-cap scenarios stay exercisable.
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

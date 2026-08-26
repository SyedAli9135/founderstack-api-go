package llm

import "testing"

func TestEstimateCostUSD(t *testing.T) {
	cases := []struct {
		name  string
		model string
		usage TokenUsage
		want  float64
	}{
		{"claude, input+output only", "claude-sonnet-4-5", TokenUsage{InputTokens: 1000, OutputTokens: 1000}, 0.003 + 0.015},
		{"gpt", "gpt-4o", TokenUsage{InputTokens: 1000, OutputTokens: 1000}, 0.0025 + 0.01},
		{"gemini, case-insensitive", "Gemini-2.0-Flash", TokenUsage{InputTokens: 1000, OutputTokens: 1000}, 0.00125 + 0.005},
		{"qwen", "qwen-max", TokenUsage{InputTokens: 1000, OutputTokens: 1000}, 0.0005 + 0.0015},
		{"deepseek", "deepseek-chat", TokenUsage{InputTokens: 1000, OutputTokens: 1000}, 0.0005 + 0.0015},
		{"unknown model falls back", "mock:tool-call-cap", TokenUsage{InputTokens: 1000, OutputTokens: 1000}, 0.001 + 0.003},
		{"zero usage is zero cost", "claude-sonnet-4-5", TokenUsage{}, 0},
		{"cached tokens discounted", "claude-sonnet-4-5", TokenUsage{CachedTokens: 1000}, 0.003 * 0.1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EstimateCostUSD(tc.model, tc.usage)
			if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("EstimateCostUSD(%q, %+v) = %v, want %v", tc.model, tc.usage, got, tc.want)
			}
		})
	}
}

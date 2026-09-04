package graph

import (
	"errors"
	"fmt"

	coremcp "github.com/founderstack/api/internal/core/mcp"
)

// PolicyScope mirrors agents.policy_scope's JSON shape — duplicated
// from internal/api/agents to avoid a circular import.
type PolicyScope struct {
	MaxToolCalls     *int32   `json:"max_tool_calls,omitempty"`
	MaxCostPerRunUSD *float64 `json:"max_cost_per_run_usd,omitempty"`
	AllowedTools     []string `json:"allowed_tools"`
}

var (
	ErrToolNotAllowed      = errors.New("graph: tool not allowed by agent's policy_scope")
	ErrToolCallCapExceeded = errors.New("graph: run exceeded agent's max_tool_calls policy")
	ErrCostCapExceeded     = errors.New("graph: run exceeded agent's max_cost_per_run_usd policy")
)

// CheckToolAllowed enforces AllowedTools against a qualified tool name
// ("service.tool_name") — an empty list denies everything by default.
func (p PolicyScope) CheckToolAllowed(qualifiedToolName string) error {
	for _, allowed := range p.AllowedTools {
		if allowed == qualifiedToolName {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrToolNotAllowed, qualifiedToolName)
}

// CheckCaps enforces MaxToolCalls/MaxCostPerRunUSD against state's
// running counters — called after every tool call, not just at node transitions.
func (p PolicyScope) CheckCaps(state *RunState) error {
	if p.MaxToolCalls != nil && int32(state.ToolCallCount) >= *p.MaxToolCalls {
		return fmt.Errorf("%w: %d/%d tool calls", ErrToolCallCapExceeded, state.ToolCallCount, *p.MaxToolCalls)
	}
	if p.MaxCostPerRunUSD != nil && state.CostSoFarUSD >= *p.MaxCostPerRunUSD {
		return fmt.Errorf("%w: $%.4f/$%.2f", ErrCostCapExceeded, state.CostSoFarUSD, *p.MaxCostPerRunUSD)
	}
	return nil
}

// RequiresApproval reports whether a tool call must route through
// approval_gate before executing — purely the tool's static RiskLevel,
// always true for RiskWriteDestructiveOrFinancial regardless of amount.
// Not policy_scope-driven: a property of the tool, not the agent calling it.
func RequiresApproval(risk coremcp.RiskLevel) bool {
	return risk == coremcp.RiskWriteDestructiveOrFinancial
}

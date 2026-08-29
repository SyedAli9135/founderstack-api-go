package graph

import (
	"errors"
	"fmt"

	coremcp "github.com/founderstack/api/internal/core/mcp"
)

// PolicyScope mirrors agents.policy_scope's JSON shape internal/api/agents/handler.go's policyScope struct
// — duplicated deliberately to avoid a circular import). It is used by the engine to enforce runtime
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
// ("service.tool_name"). An agent with an empty AllowedTools list allows noting
// the default is to be conservative and deny everything unless explicitly allowed.
// This is called at every node transition, before the engine even attempts to call the tool.
func (p PolicyScope) CheckToolAllowed(qualifiedToolName string) error {
	for _, allowed := range p.AllowedTools {
		if allowed == qualifiedToolName {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrToolNotAllowed, qualifiedToolName)
}

// CheckCaps enforces MaxToolCalls/MaxCostPerRunUSD against state's
// running counters. Called after every tool call (not just at node transitions
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
// approval_gate_node before executing, based purely on the tool's static
// RiskLevel — always true for RiskWriteDestructiveOrFinancial, regardless
// of amount or any workflow-level requires_approval/approval_conditions
// config, which only ever adds gating on top of this and can never remove
// it. This is the one guardrail decision that isn't policy_scope-driven:
// it's a property of the tool itself, not the agent calling it.
func RequiresApproval(risk coremcp.RiskLevel) bool {
	return risk == coremcp.RiskWriteDestructiveOrFinancial
}

package graph

import (
	"errors"
	"fmt"

	coremcp "github.com/founderstack/api/internal/core/mcp"
)

// PolicyScope mirrors agents.policy_scope's JSON shape
// (internal/api/agents/handler.go's policyScope struct — duplicated
// deliberately, not imported: internal/api/agents owns validating and
// storing this JSON at write time, internal/core/graph owns enforcing it
// at run time, and a security-enforcement type staying in lockstep with
// its storage type is worth a small duplication rather than a
// cross-layer import from core into api). AllowedTools entries are
// qualified "service.tool_name" strings, matching
// internal/core/mcp.Registry's own naming convention.
type PolicyScope struct {
	MaxToolCalls     *int32   `json:"max_tool_calls,omitempty"`
	MaxCostPerRunUSD *float64 `json:"max_cost_per_run_usd,omitempty"`
	AllowedTools     []string `json:"allowed_tools"`
}

var (
	// ErrToolNotAllowed means the requested tool isn't in the agent's
	// policy_scope.allowed_tools. Must be checked at the executor node
	// itself — planner intent is not a security boundary, so a
	// hallucinated or otherwise out-of-scope tool call that made it into
	// a plan is still refused here.
	ErrToolNotAllowed = errors.New("graph: tool not allowed by agent's policy_scope")
	// ErrToolCallCapExceeded / ErrCostCapExceeded mean the run tripped
	// one of policy_scope's two runtime ceilings. The engine aborts the
	// run when either fires — not silently, per the harness plan's
	// guardrail catalog — reporter_node (once built) is responsible for
	// composing a clear explanation from the returned error.
	ErrToolCallCapExceeded = errors.New("graph: run exceeded agent's max_tool_calls policy")
	ErrCostCapExceeded     = errors.New("graph: run exceeded agent's max_cost_per_run_usd policy")
)

// CheckToolAllowed enforces AllowedTools against a qualified tool name
// ("service.tool_name"). An agent with an empty AllowedTools list allows
// nothing — matches internal/api/agents/handler.go's own
// validatePolicyScope, which already rejects an empty allowed_tools list
// at write time, so an empty list reaching here would mean a data
// integrity bug elsewhere, not a legitimate "allow everything" state.
func (p PolicyScope) CheckToolAllowed(qualifiedToolName string) error {
	for _, allowed := range p.AllowedTools {
		if allowed == qualifiedToolName {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrToolNotAllowed, qualifiedToolName)
}

// CheckCaps enforces MaxToolCalls/MaxCostPerRunUSD against state's
// running counters. Called after every tool call (not just at node
// transitions), same granularity as the engine's own per-tool-call
// checkpointing — see the harness plan's "checked after every LLM/tool
// call" guardrail. A nil cap on either field means that particular limit
// is unset for this agent, not zero.
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

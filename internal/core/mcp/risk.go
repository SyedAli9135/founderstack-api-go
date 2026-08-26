package mcp

import gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

// RiskLevel classifies how much latitude a tool call gets before the
// graph engine's approval gate is required — see WORKFLOW_PLAN_GO.md's
// Workflow 9 harness planning notes. A tool's risk is a static property
// set at registration, never a runtime judgment call the engine makes.
type RiskLevel string

const (
	// RiskRead tools never require approval.
	RiskRead RiskLevel = "read"
	// RiskWriteReversible tools may require approval per a workflow's own
	// requires_approval/approval_conditions config, but are never
	// unconditionally gated.
	RiskWriteReversible RiskLevel = "write_reversible"
	// RiskWriteDestructiveOrFinancial tools ALWAYS require approval,
	// regardless of amount or any workflow-level config — pay/refund
	// actions, deletes, and external-facing irreversible publishes (see
	// servers/linkedin.go's draft_post). This is the one guardrail
	// nothing in this codebase is allowed to bypass.
	RiskWriteDestructiveOrFinancial RiskLevel = "write_destructive_or_financial"
)

// RiskLevelFor derives a tool's RiskLevel from its MCP-protocol-native
// ToolAnnotations (ReadOnlyHint/DestructiveHint) rather than a separate,
// parallel classification scheme — every tool server in this codebase is
// first-party and runs in-process, so the MCP spec's warning against
// trusting a remote/untrusted server's self-reported annotations doesn't
// apply: we wrote every annotation ourselves, at the same call site as
// the tool's schema and handler.
//
// A tool with no annotations, or DestructiveHint left nil, is treated as
// the most restrictive tier — fail closed, not open. This matches
// DestructiveHint's own documented protocol default (true when omitted):
// an unclassified or newly-added tool must never silently skip the
// approval gate because someone forgot to set Annotations.
func RiskLevelFor(annotations *gomcp.ToolAnnotations) RiskLevel {
	if annotations == nil {
		return RiskWriteDestructiveOrFinancial
	}
	if annotations.ReadOnlyHint {
		return RiskRead
	}
	if annotations.DestructiveHint == nil || *annotations.DestructiveHint {
		return RiskWriteDestructiveOrFinancial
	}
	return RiskWriteReversible
}

// ReadOnly, ReversibleWrite, and DestructiveOrFinancial build the
// *gomcp.ToolAnnotations value for each of the 3 RiskLevel tiers — every
// AddTool call in internal/core/mcp/servers sets Annotations to one of
// these, keeping each tool's classification a one-line, self-documenting
// part of its registration rather than a separate table to keep in sync.
func ReadOnly() *gomcp.ToolAnnotations {
	return &gomcp.ToolAnnotations{ReadOnlyHint: true}
}

func ReversibleWrite() *gomcp.ToolAnnotations {
	f := false
	return &gomcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &f}
}

func DestructiveOrFinancial() *gomcp.ToolAnnotations {
	t := true
	return &gomcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &t}
}

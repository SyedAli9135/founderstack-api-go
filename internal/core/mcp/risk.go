package mcp

import gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

// RiskLevel classifies how much latitude a tool call gets before the
// graph engine's approval gate is required
type RiskLevel string

const (
	RiskRead                        RiskLevel = "read"
	RiskWriteReversible             RiskLevel = "write_reversible"
	RiskWriteDestructiveOrFinancial RiskLevel = "write_destructive_or_financial"
)

// RiskLevelFor derives a tool's RiskLevel from its MCP-protocol-native
// ToolAnnotations (ReadOnlyHint/DestructiveHint) rather than a separate,
// parallel classification scheme — every tool server in this codebase is
// first-party and runs in-process, so the MCP spec's warning against
// trusting a remote/untrusted server's self-reported annotations doesn't
// apply: we wrote every annotation ourselves, at the same call site as
// the tool's schema and handler.
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
// *gomcp.ToolAnnotations value for each of the 3 RiskLevel tiers
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

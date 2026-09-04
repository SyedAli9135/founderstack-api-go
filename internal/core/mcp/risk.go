package mcp

import gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

// RiskLevel gates how much latitude a tool call gets before the graph
// engine's approval gate is required.
type RiskLevel string

const (
	RiskRead                        RiskLevel = "read"
	RiskWriteReversible             RiskLevel = "write_reversible"
	RiskWriteDestructiveOrFinancial RiskLevel = "write_destructive_or_financial"
)

// RiskLevelFor reads MCP-native ToolAnnotations directly rather than a
// parallel classification scheme — safe here since every server is
// first-party and in-process, unlike the MCP spec's remote-server case.
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

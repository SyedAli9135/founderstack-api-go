package mcp

import (
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRiskLevelFor_NoAnnotationsFailsClosed(t *testing.T) {
	if got := RiskLevelFor(nil); got != RiskWriteDestructiveOrFinancial {
		t.Fatalf("RiskLevelFor(nil) = %q, want %q (fail closed)", got, RiskWriteDestructiveOrFinancial)
	}
}

func TestRiskLevelFor_DestructiveHintNilFailsClosed(t *testing.T) {
	// A write tool that forgot to set DestructiveHint must still land on
	// the most restrictive tier — matches MCP's own documented default
	// (true when omitted), not treated as reversible by omission.
	got := RiskLevelFor(&gomcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: nil})
	if got != RiskWriteDestructiveOrFinancial {
		t.Fatalf("RiskLevelFor(nil DestructiveHint) = %q, want %q", got, RiskWriteDestructiveOrFinancial)
	}
}

func TestRiskLevelFor_AllThreeTiers(t *testing.T) {
	if got := RiskLevelFor(ReadOnly()); got != RiskRead {
		t.Errorf("ReadOnly() = %q, want %q", got, RiskRead)
	}
	if got := RiskLevelFor(ReversibleWrite()); got != RiskWriteReversible {
		t.Errorf("ReversibleWrite() = %q, want %q", got, RiskWriteReversible)
	}
	if got := RiskLevelFor(DestructiveOrFinancial()); got != RiskWriteDestructiveOrFinancial {
		t.Errorf("DestructiveOrFinancial() = %q, want %q", got, RiskWriteDestructiveOrFinancial)
	}
}

func TestRiskLevelFor_ReadOnlyWinsEvenIfDestructiveHintSet(t *testing.T) {
	// Per the MCP spec, DestructiveHint is only meaningful when
	// ReadOnlyHint is false — a read-only tool is never destructive
	// regardless of what else is set.
	destructive := true
	got := RiskLevelFor(&gomcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &destructive})
	if got != RiskRead {
		t.Fatalf("RiskLevelFor(ReadOnlyHint=true, DestructiveHint=true) = %q, want %q", got, RiskRead)
	}
}

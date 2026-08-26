package graph

import (
	"errors"
	"testing"

	coremcp "github.com/founderstack/api/internal/core/mcp"
)

func int32Ptr(v int32) *int32       { return &v }
func float64Ptr(v float64) *float64 { return &v }

func TestPolicyScope_CheckToolAllowed(t *testing.T) {
	p := PolicyScope{AllowedTools: []string{"stripe.list_subscriptions", "slack.send_message"}}

	if err := p.CheckToolAllowed("slack.send_message"); err != nil {
		t.Errorf("allowed tool rejected: %v", err)
	}
	if err := p.CheckToolAllowed("stripe.refund_payment"); !errors.Is(err, ErrToolNotAllowed) {
		t.Errorf("out-of-scope tool err = %v, want ErrToolNotAllowed", err)
	}
}

func TestPolicyScope_CheckToolAllowed_EmptyListAllowsNothing(t *testing.T) {
	p := PolicyScope{}
	if err := p.CheckToolAllowed("stripe.list_subscriptions"); !errors.Is(err, ErrToolNotAllowed) {
		t.Errorf("empty policy_scope err = %v, want ErrToolNotAllowed (fail closed)", err)
	}
}

func TestPolicyScope_CheckCaps_ToolCallCap(t *testing.T) {
	p := PolicyScope{MaxToolCalls: int32Ptr(5)}

	if err := p.CheckCaps(&RunState{ToolCallCount: 4}); err != nil {
		t.Errorf("under cap rejected: %v", err)
	}
	if err := p.CheckCaps(&RunState{ToolCallCount: 5}); !errors.Is(err, ErrToolCallCapExceeded) {
		t.Errorf("at cap err = %v, want ErrToolCallCapExceeded", err)
	}
	if err := p.CheckCaps(&RunState{ToolCallCount: 6}); !errors.Is(err, ErrToolCallCapExceeded) {
		t.Errorf("over cap err = %v, want ErrToolCallCapExceeded", err)
	}
}

func TestPolicyScope_CheckCaps_CostCap(t *testing.T) {
	p := PolicyScope{MaxCostPerRunUSD: float64Ptr(2.00)}

	if err := p.CheckCaps(&RunState{CostSoFarUSD: 1.99}); err != nil {
		t.Errorf("under cap rejected: %v", err)
	}
	if err := p.CheckCaps(&RunState{CostSoFarUSD: 2.00}); !errors.Is(err, ErrCostCapExceeded) {
		t.Errorf("at cap err = %v, want ErrCostCapExceeded", err)
	}
}

func TestPolicyScope_CheckCaps_NilCapsMeanUnset(t *testing.T) {
	p := PolicyScope{} // no caps configured
	if err := p.CheckCaps(&RunState{ToolCallCount: 1000, CostSoFarUSD: 1000}); err != nil {
		t.Errorf("nil caps should never trip: %v", err)
	}
}

func TestRequiresApproval(t *testing.T) {
	cases := []struct {
		risk coremcp.RiskLevel
		want bool
	}{
		{coremcp.RiskRead, false},
		{coremcp.RiskWriteReversible, false},
		{coremcp.RiskWriteDestructiveOrFinancial, true},
	}
	for _, c := range cases {
		if got := RequiresApproval(c.risk); got != c.want {
			t.Errorf("RequiresApproval(%q) = %v, want %v", c.risk, got, c.want)
		}
	}
}

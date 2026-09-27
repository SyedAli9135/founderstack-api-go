// Package billing holds FounderStack's own platform billing: the plan tiers,
// the Stripe calls behind them, and how a Stripe subscription's state maps
// onto an organization's plan and limits.
package billing

// Plan is one subscription tier. Limits are written onto the organization
// row whenever its subscription changes, so every existing limit check
// (agents, workflows, storage) reads the current plan without knowing
// about Stripe.
type Plan struct {
	Tier            string `json:"tier"`
	Name            string `json:"name"`
	MonthlyPriceUSD int64  `json:"monthly_price_usd"`
	LookupKey       string `json:"-"`
	MaxAgents       int32  `json:"max_agents"`
	MaxWorkflows    int32  `json:"max_workflows"`
	MaxStorageGB    int32  `json:"max_storage_gb"`
	MaxIntegrations int32  `json:"max_integrations"`
	// Client workspaces (workflow 24): IncludedClientWorkspaces come with
	// the plan; each active one beyond that costs ExtraWorkspaceUSD a month,
	// up to MaxClientWorkspaces. ExtraWorkspaceUSD 0 means a hard cap at
	// the included count.
	IncludedClientWorkspaces int32    `json:"included_client_workspaces"`
	MaxClientWorkspaces      int32    `json:"max_client_workspaces"`
	ExtraWorkspaceUSD        int64    `json:"extra_workspace_usd"`
	ExtraWorkspaceLookupKey  string   `json:"-"`
	Features                 []string `json:"features"`
}

// Plans is ordered cheapest first; the billing page renders it as-is.
// Starter's limits equal the organizations table's column defaults, so an
// org that never subscribes is on Starter limits from the start.
var Plans = []Plan{
	{
		Tier: "starter", Name: "Starter", MonthlyPriceUSD: 29, LookupKey: "founderstack_starter_monthly",
		MaxAgents: 3, MaxWorkflows: 5, MaxStorageGB: 5, MaxIntegrations: 3,
		IncludedClientWorkspaces: 1, MaxClientWorkspaces: 1,
		Features: []string{"3 agents", "5 workflows", "5 GB knowledge base", "3 integrations", "1 client workspace"},
	},
	{
		Tier: "growth", Name: "Growth", MonthlyPriceUSD: 99, LookupKey: "founderstack_growth_monthly",
		MaxAgents: 10, MaxWorkflows: 25, MaxStorageGB: 25, MaxIntegrations: 10,
		IncludedClientWorkspaces: 3, MaxClientWorkspaces: 25,
		ExtraWorkspaceUSD: 15, ExtraWorkspaceLookupKey: "founderstack_growth_extra_workspace_monthly",
		Features: []string{"10 agents", "25 workflows", "25 GB knowledge base", "10 integrations",
			"3 client workspaces included, then $15/mo each (up to 25)"},
	},
	{
		Tier: "studio", Name: "Studio", MonthlyPriceUSD: 249, LookupKey: "founderstack_studio_monthly",
		MaxAgents: 50, MaxWorkflows: 100, MaxStorageGB: 100, MaxIntegrations: 25,
		IncludedClientWorkspaces: 10, MaxClientWorkspaces: 100,
		ExtraWorkspaceUSD: 10, ExtraWorkspaceLookupKey: "founderstack_studio_extra_workspace_monthly",
		Features: []string{"50 agents", "100 workflows", "100 GB knowledge base", "25 integrations",
			"10 client workspaces included, then $10/mo each (up to 100)"},
	},
}

// DefaultPlan is what an org falls back to with no live subscription.
var DefaultPlan = Plans[0]

// PlanByTier returns the plan for a tier name.
func PlanByTier(tier string) (Plan, bool) {
	for _, p := range Plans {
		if p.Tier == tier {
			return p, true
		}
	}
	return Plan{}, false
}

// PlanByLookupKey maps a Stripe price's lookup_key back to its plan.
func PlanByLookupKey(key string) (Plan, bool) {
	for _, p := range Plans {
		if p.LookupKey == key {
			return p, true
		}
	}
	return Plan{}, false
}

// IsExtraWorkspaceLookupKey reports whether a price is one of the plans'
// "additional client workspace" prices.
func IsExtraWorkspaceLookupKey(key string) bool {
	for _, p := range Plans {
		if p.ExtraWorkspaceLookupKey != "" && p.ExtraWorkspaceLookupKey == key {
			return true
		}
	}
	return false
}

// ExtraWorkspaces is how many of active client workspaces are billed on
// top of the plan.
func (p Plan) ExtraWorkspaces(active int64) int64 {
	if p.ExtraWorkspaceUSD == 0 || active <= int64(p.IncludedClientWorkspaces) {
		return 0
	}
	return active - int64(p.IncludedClientWorkspaces)
}

// Rank orders tiers so the UI can say upgrade vs. downgrade.
func (p Plan) Rank() int {
	for i, q := range Plans {
		if q.Tier == p.Tier {
			return i
		}
	}
	return -1
}

// IsLive reports whether a Stripe subscription status still grants the
// paid plan. past_due keeps it: Stripe is still retrying the card, and
// agents shouldn't go offline without warning while the founder fixes it.
func IsLive(status string) bool {
	switch status {
	case "active", "trialing", "past_due":
		return true
	}
	return false
}

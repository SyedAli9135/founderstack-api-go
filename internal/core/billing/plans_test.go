package billing

import (
	"testing"
	"time"

	"github.com/stripe/stripe-go/v86"
)

func subOn(lookupKey string, status stripe.SubscriptionStatus) *stripe.Subscription {
	return &stripe.Subscription{
		ID: "sub_1", Status: status, Customer: &stripe.Customer{ID: "cus_1"},
		Items: &stripe.SubscriptionItemList{Data: []*stripe.SubscriptionItem{{
			ID: "si_1", Price: &stripe.Price{LookupKey: lookupKey}, CurrentPeriodEnd: 1_900_000_000,
		}}},
	}
}

func TestStateFromSubscription(t *testing.T) {
	growth, _ := PlanByTier("growth")

	st := StateFromSubscription(subOn(growth.LookupKey, stripe.SubscriptionStatusActive))
	if st.Plan.Tier != "growth" || st.Status != "active" || st.SubscriptionID != "sub_1" {
		t.Fatalf("active growth: got %+v", st)
	}
	if !st.PeriodEnd.Equal(time.Unix(1_900_000_000, 0)) {
		t.Errorf("period end = %v", st.PeriodEnd)
	}

	// past_due keeps the paid plan while Stripe retries.
	if st := StateFromSubscription(subOn(growth.LookupKey, stripe.SubscriptionStatusPastDue)); st.Plan.Tier != "growth" {
		t.Errorf("past_due should keep growth, got %s", st.Plan.Tier)
	}

	// Anything no longer live falls back to the default plan's limits.
	for _, s := range []stripe.SubscriptionStatus{
		stripe.SubscriptionStatusCanceled, stripe.SubscriptionStatusUnpaid,
		stripe.SubscriptionStatusIncomplete, stripe.SubscriptionStatusIncompleteExpired,
	} {
		if st := StateFromSubscription(subOn(growth.LookupKey, s)); st.Plan.Tier != DefaultPlan.Tier {
			t.Errorf("%s: want default plan, got %s", s, st.Plan.Tier)
		}
	}

	// A price that isn't one of ours can't grant a plan.
	if st := StateFromSubscription(subOn("someone_elses_price", stripe.SubscriptionStatusActive)); st.Plan.Tier != DefaultPlan.Tier {
		t.Errorf("unknown price: want default plan, got %s", st.Plan.Tier)
	}

	sub := subOn(growth.LookupKey, stripe.SubscriptionStatusActive)
	sub.CancelAt = 1_900_000_000
	if !StateFromSubscription(sub).CancelAtPeriodEnd {
		t.Error("a scheduled cancel_at should read as cancelling")
	}
}

func TestPlansAreConsistent(t *testing.T) {
	seen := map[string]bool{}
	for i, p := range Plans {
		if seen[p.LookupKey] || p.LookupKey == "" {
			t.Errorf("%s: duplicate or empty lookup key", p.Tier)
		}
		seen[p.LookupKey] = true
		if got, ok := PlanByLookupKey(p.LookupKey); !ok || got.Tier != p.Tier {
			t.Errorf("%s: lookup key doesn't round-trip", p.Tier)
		}
		if p.Rank() != i {
			t.Errorf("%s: rank %d, want %d", p.Tier, p.Rank(), i)
		}
		if p.IncludedClientWorkspaces > p.MaxClientWorkspaces || (p.ExtraWorkspaceUSD > 0) != (p.ExtraWorkspaceLookupKey != "") {
			t.Errorf("%s: inconsistent client workspace terms", p.Tier)
		}
		if i > 0 {
			prev := Plans[i-1]
			if p.MonthlyPriceUSD <= prev.MonthlyPriceUSD || p.MaxAgents < prev.MaxAgents || p.MaxWorkflows < prev.MaxWorkflows {
				t.Errorf("%s should cost more and allow at least as much as %s", p.Tier, prev.Tier)
			}
		}
	}
	// Starter's limits must equal the organizations column defaults, or an
	// org that never subscribes would silently differ from Starter.
	if s := DefaultPlan; s.MaxAgents != 3 || s.MaxWorkflows != 5 || s.MaxStorageGB != 5 || s.MaxIntegrations != 3 ||
		s.IncludedClientWorkspaces != 1 || s.MaxClientWorkspaces != 1 {
		t.Errorf("starter limits drifted from the column defaults: %+v", s)
	}
}

func TestWorkspaceItemChanges(t *testing.T) {
	growth, _ := PlanByTier("growth")
	studio, _ := PlanByTier("studio")
	sub := func(items ...*stripe.SubscriptionItem) *stripe.Subscription {
		base := &stripe.SubscriptionItem{ID: "si_plan", Quantity: 1, Price: &stripe.Price{LookupKey: growth.LookupKey}}
		return &stripe.Subscription{Items: &stripe.SubscriptionItemList{Data: append([]*stripe.SubscriptionItem{base}, items...)}}
	}
	extra := func(id, key string, qty int64) *stripe.SubscriptionItem {
		return &stripe.SubscriptionItem{ID: id, Quantity: qty, Price: &stripe.Price{LookupKey: key}}
	}

	cases := []struct {
		name string
		sub  *stripe.Subscription
		plan Plan
		want int64
		out  []ItemChange
	}{
		{"nothing to bill, nothing there", sub(), growth, 0, nil},
		{"first extra workspace adds the item", sub(), growth, 2, []ItemChange{{LookupKey: growth.ExtraWorkspaceLookupKey, Quantity: 2}}},
		{"already right is a no-op", sub(extra("si_x", growth.ExtraWorkspaceLookupKey, 2)), growth, 2, nil},
		{"count change updates quantity", sub(extra("si_x", growth.ExtraWorkspaceLookupKey, 2)), growth, 1, []ItemChange{{ItemID: "si_x", Quantity: 1}}},
		{"back under the included count removes it", sub(extra("si_x", growth.ExtraWorkspaceLookupKey, 1)), growth, 0, []ItemChange{{ItemID: "si_x", Delete: true}}},
		{"plan change swaps the extra price", sub(extra("si_x", growth.ExtraWorkspaceLookupKey, 3)), studio, 1,
			[]ItemChange{{ItemID: "si_x", Delete: true}, {LookupKey: studio.ExtraWorkspaceLookupKey, Quantity: 1}}},
		{"a duplicate item is removed", sub(extra("si_a", growth.ExtraWorkspaceLookupKey, 1), extra("si_b", growth.ExtraWorkspaceLookupKey, 1)), growth, 1,
			[]ItemChange{{ItemID: "si_b", Delete: true}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := workspaceItemChanges(c.sub, c.plan, c.want)
			if len(got) != len(c.out) {
				t.Fatalf("got %+v, want %+v", got, c.out)
			}
			for i := range got {
				if got[i] != c.out[i] {
					t.Fatalf("got %+v, want %+v", got, c.out)
				}
			}
		})
	}
}

func TestExtraWorkspaces(t *testing.T) {
	starter, _ := PlanByTier("starter")
	growth, _ := PlanByTier("growth")
	for _, c := range []struct {
		plan   Plan
		active int64
		want   int64
	}{{starter, 5, 0}, {growth, 0, 0}, {growth, 3, 0}, {growth, 4, 1}, {growth, 25, 22}} {
		if got := c.plan.ExtraWorkspaces(c.active); got != c.want {
			t.Errorf("%s with %d active: %d, want %d", c.plan.Tier, c.active, got, c.want)
		}
	}
}

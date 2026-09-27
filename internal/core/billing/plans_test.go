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
		if i > 0 {
			prev := Plans[i-1]
			if p.MonthlyPriceUSD <= prev.MonthlyPriceUSD || p.MaxAgents < prev.MaxAgents || p.MaxWorkflows < prev.MaxWorkflows {
				t.Errorf("%s should cost more and allow at least as much as %s", p.Tier, prev.Tier)
			}
		}
	}
	// Starter's limits must equal the organizations column defaults, or an
	// org that never subscribes would silently differ from Starter.
	if s := DefaultPlan; s.MaxAgents != 3 || s.MaxWorkflows != 5 || s.MaxStorageGB != 5 || s.MaxIntegrations != 3 {
		t.Errorf("starter limits drifted from the column defaults: %+v", s)
	}
}

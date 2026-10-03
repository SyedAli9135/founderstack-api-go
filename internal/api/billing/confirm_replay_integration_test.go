//go:build integration

package billing

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/stripe/stripe-go/v86"

	corebilling "github.com/founderstack/api/internal/core/billing"
	"github.com/founderstack/api/internal/core/billing/billingtest"
)

// The success URL carries ?session_id=..., so a refresh, a bookmark or the
// back button can replay Confirm for an old Checkout. Replaying one whose
// subscription has since been replaced (and canceled) must never knock out the
// subscription the org is on now.
func TestSubscription_ConfirmReplayOfReplacedCheckoutKeepsCurrentPlan(t *testing.T) {
	appPool, systemPool, cfg := testAppPool(t), testSystemPool(t), testConfig(t)
	fake := billingtest.New()
	router := stripeRouter(t, systemPool, appPool, cfg, fake)
	orgID, admin := testOrgAndUser(t, systemPool)
	upgrade := "/api/v1/billing/subscription/upgrade"
	confirm := "/api/v1/billing/subscription/confirm"

	// Creates the org's Stripe customer.
	authedJSON(t, router, cfg, admin, http.MethodPost, upgrade, map[string]string{"tier": "starter"})
	var customer string
	if err := systemPool.QueryRow(context.Background(), `select stripe_customer_id from organizations where id = $1`, orgID).Scan(&customer); err != nil || customer == "" {
		t.Fatalf("customer not created: %q %v", customer, err)
	}

	starter, _ := corebilling.PlanByTier("starter")
	growth, _ := corebilling.PlanByTier("growth")
	fake.Subscription("sub_old", customer, starter, stripe.SubscriptionStatusActive)
	fake.Subscription("sub_new", customer, growth, stripe.SubscriptionStatusActive)
	fake.Sessions["cs_old"] = &stripe.CheckoutSession{ClientReferenceID: orgID.String(), Status: "complete", Subscription: &stripe.Subscription{ID: "sub_old"}}
	fake.Sessions["cs_new"] = &stripe.CheckoutSession{ClientReferenceID: orgID.String(), Status: "complete", Subscription: &stripe.Subscription{ID: "sub_new"}}

	for _, session := range []string{"cs_old", "cs_new"} {
		if rec := authedJSON(t, router, cfg, admin, http.MethodPost, confirm, map[string]string{"session_id": session}); rec.Code != http.StatusOK {
			t.Fatalf("confirm %s: %d %s", session, rec.Code, rec.Body)
		}
	}
	// The second checkout replaced the first: sub_old is canceled, sub_new is current.
	if !slices.Contains(fake.Canceled, "sub_old") || slices.Contains(fake.Canceled, "sub_new") {
		t.Fatalf("after two checkouts canceled = %v, want only sub_old", fake.Canceled)
	}

	// Replay the first, now-canceled, session.
	authedJSON(t, router, cfg, admin, http.MethodPost, confirm, map[string]string{"session_id": "cs_old"})

	if slices.Contains(fake.Canceled, "sub_new") {
		t.Errorf("replaying an old checkout canceled the current subscription: canceled = %v", fake.Canceled)
	}
	var subID, tier, status string
	if err := systemPool.QueryRow(context.Background(),
		`select stripe_subscription_id, plan_tier, subscription_status from organizations where id = $1`, orgID).Scan(&subID, &tier, &status); err != nil {
		t.Fatal(err)
	}
	if subID != "sub_new" || tier != "growth" || status != "active" {
		t.Errorf("org after replay = (%s, %s, %s), want (sub_new, growth, active)", subID, tier, status)
	}
}

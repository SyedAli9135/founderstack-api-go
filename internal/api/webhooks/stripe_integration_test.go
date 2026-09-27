//go:build integration

package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/core/billing"
	"github.com/founderstack/api/internal/core/billing/billingtest"
)

const stripeTestSecret = "whsec_founderstack_webhook_test"

type stripeEnv struct {
	t      *testing.T
	pool   *pgxpool.Pool
	router *gin.Engine
	fake   *billingtest.Fake
	emails *billingtest.Emails
	suffix string
	events []string
}

func newStripeEnv(t *testing.T) *stripeEnv {
	pool := testPool(t)
	fake := billingtest.New()
	emails := &billingtest.Emails{}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())
	syncer := billing.NewSyncer(pool, fake, emails, "http://app.test")
	NewStripeHandler(pool, syncer, stripeTestSecret).Register(r.Group("/api/webhooks"))
	env := &stripeEnv{t: t, pool: pool, router: r, fake: fake, emails: emails, suffix: response.NewID()[:10]}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `delete from stripe_events where id = any($1)`, env.events)
	})
	return env
}

// org creates an org with an admin and a member, owning a Stripe customer.
func (e *stripeEnv) org(name string) (pgtype.UUID, string) {
	e.t.Helper()
	ctx := context.Background()
	customer := "cus_" + name + "_" + e.suffix
	var id pgtype.UUID
	if err := e.pool.QueryRow(ctx,
		`insert into organizations (clerk_org_id, name, slug, stripe_customer_id) values ($1, $2, $1, $3) returning id`,
		"org_stripe_"+name+"_"+e.suffix, name, customer).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	for _, role := range []string{"admin", "member"} {
		if _, err := e.pool.Exec(ctx, `insert into users (org_id, clerk_user_id, email, role) values ($1, $2, $3, $4)`,
			id, "user_stripe_"+role+"_"+name+"_"+e.suffix, role+"-"+name+"@example.com", role); err != nil {
			e.t.Fatal(err)
		}
	}
	e.t.Cleanup(func() { _, _ = e.pool.Exec(context.Background(), `delete from organizations where id = $1`, id) })
	return id, customer
}

func (e *stripeEnv) send(eventID, eventType string, object any) *httptest.ResponseRecorder {
	e.t.Helper()
	e.events = append(e.events, eventID)
	raw, _ := json.Marshal(object)
	payload, _ := json.Marshal(map[string]any{
		"id": eventID, "object": "event", "type": eventType, "api_version": "2020-08-27",
		"data": map[string]json.RawMessage{"object": raw},
	})
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: stripeTestSecret, Timestamp: time.Now()})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/stripe", bytes.NewReader(signed.Payload))
	req.Header.Set("Stripe-Signature", signed.Header)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func (e *stripeEnv) evt(n string) string { return fmt.Sprintf("evt_%s_%s", n, e.suffix) }

type orgState struct {
	tier, status, subID string
	maxAgents           int32
	periodEnd           pgtype.Timestamptz
}

func (e *stripeEnv) state(id pgtype.UUID) orgState {
	e.t.Helper()
	var s orgState
	var subID *string
	if err := e.pool.QueryRow(context.Background(),
		`select plan_tier, subscription_status, stripe_subscription_id, max_agents, current_period_end from organizations where id = $1`, id,
	).Scan(&s.tier, &s.status, &subID, &s.maxAgents, &s.periodEnd); err != nil {
		e.t.Fatal(err)
	}
	if subID != nil {
		s.subID = *subID
	}
	return s
}

func plan(tier string) billing.Plan {
	p, _ := billing.PlanByTier(tier)
	return p
}

func TestStripeWebhook_RejectsBadSignature(t *testing.T) {
	env := newStripeEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/stripe", bytes.NewReader([]byte(`{"id":"evt_forged","type":"customer.subscription.updated"}`)))
	req.Header.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=deadbeef", time.Now().Unix()))
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forged event: want 400, got %d", rec.Code)
	}
}

func TestStripeWebhook_SubscriptionLifecycle(t *testing.T) {
	env := newStripeEnv(t)
	orgID, customer := env.org("lifecycle")
	subID := "sub_life_" + env.suffix
	sub := env.fake.Subscription(subID, customer, plan("growth"), stripe.SubscriptionStatusActive)

	if rec := env.send(env.evt("created"), "customer.subscription.created", sub); rec.Code != http.StatusOK {
		t.Fatalf("created: %d %s", rec.Code, rec.Body)
	}
	if s := env.state(orgID); s.tier != "growth" || s.status != "active" || s.subID != subID || s.maxAgents != 10 || !s.periodEnd.Valid {
		t.Fatalf("after created: %+v", s)
	}

	// The payload is only a pointer: state comes from a fresh fetch, so a
	// payload claiming another plan changes nothing.
	lying := *sub
	lying.Status = stripe.SubscriptionStatusCanceled
	fetches := env.fake.Fetches
	env.send(env.evt("stale"), "customer.subscription.updated", &lying)
	if s := env.state(orgID); s.status != "active" || env.fake.Fetches != fetches+1 {
		t.Fatalf("payload should not be trusted: %+v", s)
	}

	// A redelivered event is acknowledged without being processed again.
	fetches = env.fake.Fetches
	if rec := env.send(env.evt("stale"), "customer.subscription.updated", &lying); rec.Code != http.StatusOK || env.fake.Fetches != fetches {
		t.Fatalf("duplicate: code %d, fetches %d -> %d", rec.Code, fetches, env.fake.Fetches)
	}

	// Cancellation drops the org to Starter limits.
	sub.Status = stripe.SubscriptionStatusCanceled
	env.send(env.evt("deleted"), "customer.subscription.deleted", sub)
	if s := env.state(orgID); s.tier != "starter" || s.status != "canceled" || s.maxAgents != 3 {
		t.Fatalf("after deleted: %+v", s)
	}
}

func TestStripeWebhook_CheckoutReplacesLiveSubscription(t *testing.T) {
	env := newStripeEnv(t)
	orgID, customer := env.org("double")
	oldID, newID := "sub_old_"+env.suffix, "sub_new_"+env.suffix
	oldSub := env.fake.Subscription(oldID, customer, plan("growth"), stripe.SubscriptionStatusActive)
	env.send(env.evt("old"), "customer.subscription.created", oldSub)

	// A second live subscription's own event doesn't take over on its own...
	newSub := env.fake.Subscription(newID, customer, plan("studio"), stripe.SubscriptionStatusActive)
	env.send(env.evt("newcreated"), "customer.subscription.created", newSub)
	if s := env.state(orgID); s.subID != oldID {
		t.Fatalf("an unadopted subscription replaced the current one: %+v", s)
	}

	// ...but its completed Checkout does, and the old one is canceled so
	// the founder isn't billed twice.
	env.send(env.evt("checkout"), "checkout.session.completed", &stripe.CheckoutSession{
		ID: "cs_" + env.suffix, Mode: stripe.CheckoutSessionModeSubscription,
		ClientReferenceID: orgID.String(), Customer: &stripe.Customer{ID: customer}, Subscription: &stripe.Subscription{ID: newID},
	})
	if s := env.state(orgID); s.subID != newID || s.tier != "studio" {
		t.Fatalf("after checkout: %+v", s)
	}
	if len(env.fake.Canceled) != 1 || env.fake.Canceled[0] != oldID {
		t.Fatalf("old subscription should be canceled, got %v", env.fake.Canceled)
	}

	// The old subscription's deletion event must not undo the new plan.
	env.send(env.evt("olddeleted"), "customer.subscription.deleted", oldSub)
	if s := env.state(orgID); s.subID != newID || s.tier != "studio" || s.status != "active" {
		t.Fatalf("stale deletion overwrote the plan: %+v", s)
	}
}

func TestStripeWebhook_PaymentFailedAndRecovered(t *testing.T) {
	env := newStripeEnv(t)
	orgID, customer := env.org("pastdue")
	subID := "sub_pd_" + env.suffix
	sub := env.fake.Subscription(subID, customer, plan("growth"), stripe.SubscriptionStatusActive)
	env.send(env.evt("created"), "customer.subscription.created", sub)

	sub.Status = stripe.SubscriptionStatusPastDue
	invoice := &stripe.Invoice{
		ID: "in_" + env.suffix, AmountDue: 9900, Customer: &stripe.Customer{ID: customer},
		Parent: &stripe.InvoiceParent{SubscriptionDetails: &stripe.InvoiceParentSubscriptionDetails{Subscription: &stripe.Subscription{ID: subID}}},
	}
	if rec := env.send(env.evt("failed"), "invoice.payment_failed", invoice); rec.Code != http.StatusOK {
		t.Fatalf("payment_failed: %d %s", rec.Code, rec.Body)
	}
	// past_due keeps the paid plan while Stripe retries.
	if s := env.state(orgID); s.status != "past_due" || s.tier != "growth" {
		t.Fatalf("after failure: %+v", s)
	}
	// Owners/admins only, once — the redelivery doesn't email again.
	env.send(env.evt("failed"), "invoice.payment_failed", invoice)
	if env.emails.Count() != 1 || env.emails.Sent[0] != "admin-pastdue@example.com" {
		t.Fatalf("emails: %v", env.emails.Sent)
	}

	sub.Status = stripe.SubscriptionStatusActive
	env.send(env.evt("paid"), "invoice.paid", invoice)
	if s := env.state(orgID); s.status != "active" {
		t.Fatalf("after invoice.paid: %+v", s)
	}
}

func TestStripeWebhook_UnknownCustomerAndIrrelevantEvents(t *testing.T) {
	env := newStripeEnv(t)
	stranger := env.fake.Subscription("sub_stranger_"+env.suffix, "cus_nobody_"+env.suffix, plan("growth"), stripe.SubscriptionStatusActive)
	if rec := env.send(env.evt("stranger"), "customer.subscription.updated", stranger); rec.Code != http.StatusOK {
		t.Fatalf("unknown customer should be acknowledged, got %d", rec.Code)
	}
	if rec := env.send(env.evt("other"), "charge.refunded", map[string]string{"id": "ch_1"}); rec.Code != http.StatusOK {
		t.Fatalf("unhandled event type should be acknowledged, got %d", rec.Code)
	}
}

func TestStripeWebhook_InvoiceUpcomingWarnsAboutExtraWorkspaces(t *testing.T) {
	env := newStripeEnv(t)
	orgID, customer := env.org("upcoming")
	ctx := context.Background()
	if _, err := env.pool.Exec(ctx, `update organizations set organization_type = 'practice', plan_tier = 'growth',
		included_client_workspaces = 3, max_client_workspaces = 25 where id = $1`, orgID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(context.Background(), `delete from organizations where parent_practice_id = $1`, orgID)
	})
	addClients := func(n int) {
		for i := range n {
			id := fmt.Sprintf("org_upcoming_%s_%d_%d", env.suffix, n, i)
			if _, err := env.pool.Exec(ctx, `insert into organizations (clerk_org_id, name, slug, organization_type, parent_practice_id)
				values ($1, 'Client', $1, 'client_workspace', $2)`, id, orgID); err != nil {
				t.Fatal(err)
			}
		}
	}
	upcoming := &stripe.Invoice{Customer: &stripe.Customer{ID: customer}}

	addClients(3) // exactly the included count: nothing to warn about
	env.send(env.evt("up1"), "invoice.upcoming", upcoming)
	if env.emails.Count() != 0 {
		t.Fatalf("no extra workspaces should mean no email, got %v", env.emails.Sent)
	}
	addClients(1)
	if rec := env.send(env.evt("up2"), "invoice.upcoming", upcoming); rec.Code != http.StatusOK {
		t.Fatalf("invoice.upcoming: %d %s", rec.Code, rec.Body)
	}
	if env.emails.Count() != 1 || env.emails.Sent[0] != "admin-upcoming@example.com" {
		t.Fatalf("overage warning: %v", env.emails.Sent)
	}
}

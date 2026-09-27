//go:build integration

package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v86"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/config"
	corebilling "github.com/founderstack/api/internal/core/billing"
	"github.com/founderstack/api/internal/core/billing/billingtest"
	"github.com/founderstack/api/internal/pkg/devtoken"
)

func stripeRouter(t *testing.T, systemPool, appPool *pgxpool.Pool, cfg *config.Config, fake *billingtest.Fake) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())
	rg := r.Group("/api/v1")
	rg.Use(middleware.RequireAuth(systemPool, cfg))
	syncer := corebilling.NewSyncer(systemPool, fake, &billingtest.Emails{}, "http://app.test")
	NewHandler(appPool, systemPool, fake, syncer, "http://app.test").Register(rg)
	return r
}

func authedJSON(t *testing.T, router *gin.Engine, cfg *config.Config, clerkUserID, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	token, err := devtoken.Sign(cfg.DevTokenSecret.Expose(), clerkUserID)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

type envelopeOf[T any] struct {
	Data  T `json:"data"`
	Error *struct {
		Code string `json:"code"`
	} `json:"error"`
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) envelopeOf[T] {
	t.Helper()
	var e envelopeOf[T]
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return e
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	e := decode[json.RawMessage](t, rec)
	if e.Error == nil {
		return ""
	}
	return e.Error.Code
}

func addUser(t *testing.T, systemPool *pgxpool.Pool, orgID pgtype.UUID, role string) string {
	t.Helper()
	id := "user_billing_" + role + "_" + strings.ReplaceAll(orgID.String()[:8], "-", "")
	if _, err := systemPool.Exec(context.Background(),
		`insert into users (org_id, clerk_user_id, email, role) values ($1, $2, $3, $4)`,
		orgID, id, role+"@example.com", role); err != nil {
		t.Fatal(err)
	}
	return id
}

func orgLimits(t *testing.T, systemPool *pgxpool.Pool, orgID pgtype.UUID) (tier, status string, maxAgents, maxWorkflows int32) {
	t.Helper()
	if err := systemPool.QueryRow(context.Background(),
		`select plan_tier, subscription_status, max_agents, max_workflows from organizations where id = $1`, orgID,
	).Scan(&tier, &status, &maxAgents, &maxWorkflows); err != nil {
		t.Fatal(err)
	}
	return
}

func TestSubscription_TrialOrgView(t *testing.T) {
	appPool, systemPool, cfg := testAppPool(t), testSystemPool(t), testConfig(t)
	router := stripeRouter(t, systemPool, appPool, cfg, billingtest.New())
	orgID, admin := testOrgAndUser(t, systemPool)
	if _, err := systemPool.Exec(context.Background(),
		`insert into documents (org_id, filename, s3_path, byte_size) values ($1, 'a.pdf', 'k', 1000), ($1, 'b.pdf', 'k2', 500)`, orgID); err != nil {
		t.Fatal(err)
	}

	rec := authedJSON(t, router, cfg, admin, http.MethodGet, "/api/v1/billing/subscription", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	v := decode[subscriptionView](t, rec).Data
	if v.Status != "trial" || v.Plan.Tier != "starter" || v.TrialEndsAt == nil || v.NextBillingDate != nil {
		t.Errorf("trial view: %+v", v)
	}
	if !v.CanManage || v.ManagedByPractice || !v.BillingConfigured || v.HasBillingAccount {
		t.Errorf("flags: %+v", v)
	}
	if v.Usage.Agents.Limit != 3 || v.Usage.Workflows.Limit != 5 || v.Usage.StorageBytes != 1500 || v.Usage.StorageLimitGB != 5 {
		t.Errorf("usage: %+v", v.Usage)
	}
	if len(v.Plans) != 3 {
		t.Errorf("plans: %d", len(v.Plans))
	}

	// An expired trial says so.
	if _, err := systemPool.Exec(context.Background(), `update organizations set trial_ends_at = now() - interval '1 day' where id = $1`, orgID); err != nil {
		t.Fatal(err)
	}
	v = decode[subscriptionView](t, authedJSON(t, router, cfg, admin, http.MethodGet, "/api/v1/billing/subscription", nil)).Data
	if v.Status != "trial_expired" {
		t.Errorf("want trial_expired, got %s", v.Status)
	}

	// A member sees the plan but can't manage it.
	member := addUser(t, systemPool, orgID, "member")
	v = decode[subscriptionView](t, authedJSON(t, router, cfg, member, http.MethodGet, "/api/v1/billing/subscription", nil)).Data
	if v.CanManage {
		t.Error("member should not be able to manage billing")
	}
}

func TestSubscription_UpgradeCheckoutConfirmAndChange(t *testing.T) {
	appPool, systemPool, cfg := testAppPool(t), testSystemPool(t), testConfig(t)
	fake := billingtest.New()
	router := stripeRouter(t, systemPool, appPool, cfg, fake)
	orgID, admin := testOrgAndUser(t, systemPool)
	path := "/api/v1/billing/subscription/upgrade"

	if code := errCode(t, authedJSON(t, router, cfg, admin, http.MethodPost, path, map[string]string{"tier": "platinum"})); code != "UNKNOWN_TIER" {
		t.Errorf("unknown tier: %s", code)
	}
	member := addUser(t, systemPool, orgID, "member")
	if rec := authedJSON(t, router, cfg, member, http.MethodPost, path, map[string]string{"tier": "growth"}); rec.Code != http.StatusForbidden {
		t.Errorf("member upgrade: %d", rec.Code)
	}

	// No subscription yet: a Checkout session on a newly created customer.
	rec := authedJSON(t, router, cfg, admin, http.MethodPost, path, map[string]string{"tier": "growth"})
	if rec.Code != http.StatusOK {
		t.Fatalf("upgrade: %d %s", rec.Code, rec.Body)
	}
	up := decode[upgradeResponse](t, rec).Data
	if up.CheckoutURL == "" || up.Changed {
		t.Fatalf("want a checkout url: %+v", up)
	}
	co := fake.Checkouts[0]
	if co.OrgID != orgID.String() || co.Plan.Tier != "growth" ||
		co.SuccessURL != "http://app.test/settings/billing?payment=success&session_id={CHECKOUT_SESSION_ID}" {
		t.Errorf("checkout params: %+v", co)
	}
	var customer string
	_ = systemPool.QueryRow(context.Background(), `select stripe_customer_id from organizations where id = $1`, orgID).Scan(&customer)
	if customer == "" || customer != co.CustomerID {
		t.Fatalf("customer not saved: %q vs %q", customer, co.CustomerID)
	}
	// A second click reuses the same customer.
	authedJSON(t, router, cfg, admin, http.MethodPost, path, map[string]string{"tier": "growth"})
	if fake.Checkouts[1].CustomerID != customer {
		t.Error("second checkout made a new customer")
	}

	// Confirm: another org's session is indistinguishable from a missing one.
	growth, _ := corebilling.PlanByTier("growth")
	fake.Subscription("sub_a", customer, growth, stripe.SubscriptionStatusActive)
	fake.Sessions["cs_other"] = &stripe.CheckoutSession{ClientReferenceID: "someone-else", Status: "complete", Subscription: &stripe.Subscription{ID: "sub_a"}}
	fake.Sessions["cs_open"] = &stripe.CheckoutSession{ClientReferenceID: orgID.String(), Status: "open"}
	fake.Sessions["cs_ok"] = &stripe.CheckoutSession{ClientReferenceID: orgID.String(), Status: "complete", Subscription: &stripe.Subscription{ID: "sub_a"}}
	confirm := "/api/v1/billing/subscription/confirm"
	if code := errCode(t, authedJSON(t, router, cfg, admin, http.MethodPost, confirm, map[string]string{"session_id": "cs_other"})); code != "CHECKOUT_NOT_FOUND" {
		t.Errorf("foreign session: %s", code)
	}
	if code := errCode(t, authedJSON(t, router, cfg, admin, http.MethodPost, confirm, map[string]string{"session_id": "cs_open"})); code != "CHECKOUT_NOT_COMPLETE" {
		t.Errorf("open session: %s", code)
	}
	rec = authedJSON(t, router, cfg, admin, http.MethodPost, confirm, map[string]string{"session_id": "cs_ok"})
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body)
	}
	v := decode[subscriptionView](t, rec).Data
	if v.Status != "active" || v.Plan.Tier != "growth" || v.NextBillingDate == nil || v.Usage.Agents.Limit != 10 {
		t.Errorf("after confirm: %+v", v)
	}
	if tier, status, agents, workflows := orgLimits(t, systemPool, orgID); tier != "growth" || status != "active" || agents != 10 || workflows != 25 {
		t.Errorf("org row: %s %s %d %d", tier, status, agents, workflows)
	}

	// With a live subscription, upgrading changes the plan in place — no
	// second Checkout, so no second subscription.
	if code := errCode(t, authedJSON(t, router, cfg, admin, http.MethodPost, path, map[string]string{"tier": "growth"})); code != "ALREADY_ON_PLAN" {
		t.Errorf("same tier: %s", code)
	}
	rec = authedJSON(t, router, cfg, admin, http.MethodPost, path, map[string]string{"tier": "studio"})
	up = decode[upgradeResponse](t, rec).Data
	if rec.Code != http.StatusOK || !up.Changed || up.CheckoutURL != "" || up.Subscription.Plan.Tier != "studio" {
		t.Fatalf("in-place change: %d %+v", rec.Code, up)
	}
	if len(fake.Checkouts) != 2 {
		t.Errorf("in-place change must not open Checkout (checkouts=%d)", len(fake.Checkouts))
	}
	if _, _, agents, _ := orgLimits(t, systemPool, orgID); agents != 50 {
		t.Errorf("studio agents limit = %d", agents)
	}
}

func TestSubscription_PortalAndGuards(t *testing.T) {
	appPool, systemPool, cfg := testAppPool(t), testSystemPool(t), testConfig(t)
	router := stripeRouter(t, systemPool, appPool, cfg, billingtest.New())
	orgID, admin := testOrgAndUser(t, systemPool)

	if code := errCode(t, authedJSON(t, router, cfg, admin, http.MethodPost, "/api/v1/billing/portal", nil)); code != "NO_BILLING_ACCOUNT" {
		t.Errorf("portal without customer: %s", code)
	}
	if _, err := systemPool.Exec(context.Background(), `update organizations set stripe_customer_id = $2 where id = $1`, orgID, "cus_portal_"+orgID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	rec := authedJSON(t, router, cfg, admin, http.MethodPost, "/api/v1/billing/portal", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "billing.stripe.test") {
		t.Errorf("portal: %d %s", rec.Code, rec.Body)
	}

	// A client workspace is billed through its practice.
	practiceID, _ := testOrgAndUser(t, systemPool)
	if _, err := systemPool.Exec(context.Background(), `update organizations set organization_type = 'practice' where id = $1`, practiceID); err != nil {
		t.Fatal(err)
	}
	if _, err := systemPool.Exec(context.Background(),
		`update organizations set organization_type = 'client_workspace', parent_practice_id = $2 where id = $1`, orgID, practiceID); err != nil {
		t.Fatal(err)
	}
	if code := errCode(t, authedJSON(t, router, cfg, admin, http.MethodPost, "/api/v1/billing/subscription/upgrade", map[string]string{"tier": "growth"})); code != "BILLING_MANAGED_BY_PRACTICE" {
		t.Errorf("client workspace upgrade: %s", code)
	}
	v := decode[subscriptionView](t, authedJSON(t, router, cfg, admin, http.MethodGet, "/api/v1/billing/subscription", nil)).Data
	if !v.ManagedByPractice || v.CanManage {
		t.Errorf("client workspace view: %+v", v)
	}

	// Without Stripe configured, reads still work and writes say why not.
	unconfigured := testRouter(t, systemPool, appPool, cfg)
	if _, err := systemPool.Exec(context.Background(), `update organizations set organization_type = 'standard', parent_practice_id = null where id = $1`, orgID); err != nil {
		t.Fatal(err)
	}
	if code := errCode(t, authedJSON(t, unconfigured, cfg, admin, http.MethodPost, "/api/v1/billing/subscription/upgrade", map[string]string{"tier": "growth"})); code != "BILLING_NOT_CONFIGURED" {
		t.Errorf("unconfigured: %s", code)
	}
	if rec := authedJSON(t, unconfigured, cfg, admin, http.MethodGet, "/api/v1/billing/subscription", nil); rec.Code != http.StatusOK {
		t.Errorf("unconfigured read: %d", rec.Code)
	}
}

func TestSubscription_PracticeWorkspacesAndDowngradeGuard(t *testing.T) {
	appPool, systemPool, cfg := testAppPool(t), testSystemPool(t), testConfig(t)
	fake := billingtest.New()
	router := stripeRouter(t, systemPool, appPool, cfg, fake)
	orgID, admin := testOrgAndUser(t, systemPool)
	ctx := context.Background()

	growth, _ := corebilling.PlanByTier("growth")
	customer := "cus_practice_" + orgID.String()[:8]
	fake.Subscription("sub_practice_"+orgID.String()[:8], customer, growth, stripe.SubscriptionStatusActive)
	if _, err := systemPool.Exec(ctx, `update organizations set organization_type = 'practice', plan_tier = 'growth',
		subscription_status = 'active', stripe_customer_id = $2, stripe_subscription_id = $3,
		current_period_end = now() + interval '20 days', included_client_workspaces = 3, max_client_workspaces = 25
		where id = $1`, orgID, customer, "sub_practice_"+orgID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		suffix := orgID.String()[:8] + string(rune('a'+i))
		if _, err := systemPool.Exec(ctx, `insert into organizations (clerk_org_id, name, slug, organization_type, parent_practice_id)
			values ($1, 'Client', $1, 'client_workspace', $2)`, "org_client_"+suffix, orgID); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), `delete from organizations where parent_practice_id = $1`, orgID)
	})

	// 5 active on Growth: 3 included + 2 at $15 on top of $99.
	v := decode[subscriptionView](t, authedJSON(t, router, cfg, admin, http.MethodGet, "/api/v1/billing/subscription", nil)).Data
	cw := v.ClientWorkspaces
	if cw == nil || cw.Active != 5 || cw.Included != 3 || cw.Extra != 2 || cw.ExtraWorkspaceUSD != 15 || cw.Max != 25 {
		t.Fatalf("client workspaces: %+v", cw)
	}
	if v.NextInvoiceEstimateUSD == nil || *v.NextInvoiceEstimateUSD != 99+2*15 {
		t.Fatalf("next invoice estimate: %v", v.NextInvoiceEstimateUSD)
	}

	// Starter allows 1: switching would stop billing 4 running workspaces.
	rec := authedJSON(t, router, cfg, admin, http.MethodPost, "/api/v1/billing/subscription/upgrade", map[string]string{"tier": "starter"})
	if code := errCode(t, rec); code != "TOO_MANY_CLIENT_WORKSPACES" || !strings.Contains(rec.Body.String(), "Remove 4") {
		t.Fatalf("downgrade over the cap: %s %s", code, rec.Body)
	}
	// Studio has room, so it goes through.
	if rec := authedJSON(t, router, cfg, admin, http.MethodPost, "/api/v1/billing/subscription/upgrade", map[string]string{"tier": "studio"}); rec.Code != http.StatusOK {
		t.Fatalf("upgrade to studio: %d %s", rec.Code, rec.Body)
	}

	// A standard org has no client workspace block at all.
	if _, err := systemPool.Exec(ctx, `delete from organizations where parent_practice_id = $1`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := systemPool.Exec(ctx, `update organizations set organization_type = 'standard' where id = $1`, orgID); err != nil {
		t.Fatal(err)
	}
	if v := decode[subscriptionView](t, authedJSON(t, router, cfg, admin, http.MethodGet, "/api/v1/billing/subscription", nil)).Data; v.ClientWorkspaces != nil {
		t.Fatalf("standard org shouldn't have a client workspace block: %+v", v.ClientWorkspaces)
	}
}

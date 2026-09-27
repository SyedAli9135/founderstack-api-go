package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	corebilling "github.com/founderstack/api/internal/core/billing"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

type usageMeter struct {
	Used  int64 `json:"used"`
	Limit int32 `json:"limit"`
}

type planUsage struct {
	Agents         usageMeter `json:"agents"`
	Workflows      usageMeter `json:"workflows"`
	Integrations   usageMeter `json:"integrations"`
	StorageBytes   int64      `json:"storage_bytes"`
	StorageLimitGB int32      `json:"storage_limit_gb"`
}

type subscriptionView struct {
	Plan corebilling.Plan `json:"plan"`
	// Status is Stripe's subscription status, or "trial"/"trial_expired"
	// for an org that has never subscribed.
	Status            string    `json:"status"`
	TrialEndsAt       *string   `json:"trial_ends_at"`
	NextBillingDate   *string   `json:"next_billing_date"`
	CancelAtPeriodEnd bool      `json:"cancel_at_period_end"`
	HasBillingAccount bool      `json:"has_billing_account"`
	CanManage         bool      `json:"can_manage"`
	ManagedByPractice bool      `json:"managed_by_practice"`
	BillingConfigured bool      `json:"billing_configured"`
	Usage             planUsage `json:"usage"`
	// ClientWorkspaces is set for a practice (workflow 24).
	ClientWorkspaces *clientWorkspaceUsage `json:"client_workspaces,omitempty"`
	// NextInvoiceEstimateUSD is the plan plus any extra client workspaces
	// at the current count — an estimate: prorations for mid-period
	// changes and taxes aren't included.
	NextInvoiceEstimateUSD *int64             `json:"next_invoice_estimate_usd"`
	Plans                  []corebilling.Plan `json:"plans"`
}

type clientWorkspaceUsage struct {
	Active            int64 `json:"active"`
	Included          int32 `json:"included"`
	Max               int32 `json:"max"`
	Extra             int64 `json:"extra"`
	ExtraWorkspaceUSD int64 `json:"extra_workspace_usd"`
}

// Subscription is readable by every member (they see the same plan limits
// they're held to); only owners and admins can act on it.
func (h *Handler) Subscription(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	view, err := h.subscriptionView(c.Request.Context(), user)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load subscription")
		return
	}
	response.OK(c, http.StatusOK, "", view)
}

func (h *Handler) subscriptionView(ctx context.Context, user authctx.User) (subscriptionView, error) {
	// The org row is read through app_system (still pinned to the caller's
	// own org): its active client workspace count spans child orgs, which
	// RLS hides from app_user.
	org, err := dbgen.New(h.systemPool).GetOrgBilling(ctx, user.OrgID)
	if err != nil {
		return subscriptionView{}, err
	}
	var usage dbgen.GetOrgBillingUsageRow
	err = tenant.WithTx(ctx, h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		usage, err = q.GetOrgBillingUsage(ctx, user.OrgID)
		return err
	})
	if err != nil {
		return subscriptionView{}, err
	}

	plan, ok := corebilling.PlanByTier(deref(org.PlanTier))
	if !ok {
		plan = corebilling.DefaultPlan
	}
	status := deref(org.SubscriptionStatus)
	if deref(org.StripeSubscriptionID) == "" {
		status = "trial"
		if org.TrialEndsAt.Valid && org.TrialEndsAt.Time.Before(time.Now()) {
			status = "trial_expired"
		}
	}
	managed := org.OrganizationType == "client_workspace"
	view := subscriptionView{
		Plan: plan, Status: status, CancelAtPeriodEnd: org.CancelAtPeriodEnd,
		HasBillingAccount: deref(org.StripeCustomerID) != "",
		CanManage:         user.IsOwnerOrAdmin() && !managed,
		ManagedByPractice: managed,
		BillingConfigured: h.stripe != nil,
		Usage: planUsage{
			Agents:         usageMeter{Used: usage.Agents, Limit: derefInt32(org.MaxAgents)},
			Workflows:      usageMeter{Used: usage.Workflows, Limit: derefInt32(org.MaxWorkflows)},
			Integrations:   usageMeter{Used: usage.Integrations, Limit: derefInt32(org.MaxMcpIntegrations)},
			StorageBytes:   usage.StorageBytes,
			StorageLimitGB: derefInt32(org.MaxRagStorageGb),
		},
		Plans: corebilling.Plans,
	}
	if status == "trial" || status == "trial_expired" || status == "trialing" {
		view.TrialEndsAt = timeString(org.TrialEndsAt)
	}
	var extra int64
	if org.OrganizationType == "practice" {
		extra = plan.ExtraWorkspaces(org.ActiveClientWorkspaces)
		view.ClientWorkspaces = &clientWorkspaceUsage{
			Active: org.ActiveClientWorkspaces, Included: org.IncludedClientWorkspaces, Max: org.MaxClientWorkspaces,
			Extra: extra, ExtraWorkspaceUSD: plan.ExtraWorkspaceUSD,
		}
	}
	if corebilling.IsLive(status) {
		view.NextBillingDate = timeString(org.CurrentPeriodEnd)
		if !org.CancelAtPeriodEnd {
			estimate := plan.MonthlyPriceUSD + extra*plan.ExtraWorkspaceUSD
			view.NextInvoiceEstimateUSD = &estimate
		}
	}
	return view, nil
}

type upgradeRequest struct {
	Tier string `json:"tier" binding:"required"`
}

type upgradeResponse struct {
	CheckoutURL  string            `json:"checkout_url,omitempty"`
	Changed      bool              `json:"changed"`
	Subscription *subscriptionView `json:"subscription,omitempty"`
}

// Upgrade starts a subscription through Stripe Checkout — the card is
// only ever entered on Stripe's page — or, when the org already has a
// live subscription, switches its plan in place (with proration) so the
// founder is never billed twice.
func (h *Handler) Upgrade(c *gin.Context) {
	user, ok := h.requireManager(c)
	if !ok {
		return
	}
	var req upgradeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "tier is required")
		return
	}
	plan, ok := corebilling.PlanByTier(req.Tier)
	if !ok {
		response.Fail(c, http.StatusBadRequest, "UNKNOWN_TIER", "tier must be starter, growth, or studio")
		return
	}
	ctx := c.Request.Context()
	q := dbgen.New(h.systemPool)
	org, err := q.GetOrgBilling(ctx, user.OrgID)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load subscription")
		return
	}

	// A practice can't move to a plan that allows fewer client workspaces
	// than it's running — that would silently stop billing for the rest.
	if org.ActiveClientWorkspaces > int64(plan.MaxClientWorkspaces) {
		response.Fail(c, http.StatusConflict, "TOO_MANY_CLIENT_WORKSPACES", fmt.Sprintf(
			"%s allows %d client workspace%s and you have %d active. Remove %d before switching.",
			plan.Name, plan.MaxClientWorkspaces, plural(int64(plan.MaxClientWorkspaces)),
			org.ActiveClientWorkspaces, org.ActiveClientWorkspaces-int64(plan.MaxClientWorkspaces)))
		return
	}

	if subID := deref(org.StripeSubscriptionID); subID != "" && corebilling.IsLive(deref(org.SubscriptionStatus)) {
		sub, err := h.stripe.GetSubscription(ctx, subID)
		if err != nil {
			failStripe(c, err)
			return
		}
		if item := corebilling.PlanItem(sub); item != nil && item.Price.LookupKey == plan.LookupKey {
			response.Fail(c, http.StatusConflict, "ALREADY_ON_PLAN", "You're already on the "+plan.Name+" plan")
			return
		}
		updated, err := h.stripe.ChangePlan(ctx, sub, plan)
		if err != nil {
			failStripe(c, err)
			return
		}
		if _, err := h.syncer.Apply(ctx, user.OrgID, updated, true); err != nil {
			response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Plan changed in Stripe but not saved yet — it will sync within a minute")
			return
		}
		view, err := h.subscriptionView(ctx, user)
		if err != nil {
			response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load subscription")
			return
		}
		response.OK(c, http.StatusOK, "Plan changed to "+plan.Name, upgradeResponse{Changed: true, Subscription: &view})
		return
	}

	customerID := deref(org.StripeCustomerID)
	if customerID == "" {
		email, _ := q.GetUserEmailByID(ctx, user.ID)
		customerID, err = h.stripe.CreateCustomer(ctx, user.OrgID.String(), org.Name, email)
		if err != nil {
			failStripe(c, err)
			return
		}
		if err := q.SetOrgStripeCustomer(ctx, dbgen.SetOrgStripeCustomerParams{ID: user.OrgID, StripeCustomerID: &customerID}); err != nil {
			response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not save billing account")
			return
		}
	}
	billingURL := h.frontendURL + "/settings/billing"
	checkoutURL, err := h.stripe.CreateCheckoutSession(ctx, corebilling.CheckoutParams{
		OrgID: user.OrgID.String(), CustomerID: customerID, Plan: plan,
		// Stripe substitutes {CHECKOUT_SESSION_ID} itself.
		SuccessURL: billingURL + "?payment=success&session_id={CHECKOUT_SESSION_ID}",
		CancelURL:  billingURL + "?payment=cancelled",
	})
	if err != nil {
		failStripe(c, err)
		return
	}
	response.OK(c, http.StatusOK, "", upgradeResponse{CheckoutURL: checkoutURL})
}

type confirmRequest struct {
	SessionID string `json:"session_id" binding:"required"`
}

// Confirm applies a just-completed Checkout straight away when the
// founder lands back on the billing page, so the new plan shows even if
// the webhook is slow or (in local dev) not forwarded at all. The webhook
// applying the same subscription afterwards is a no-op.
func (h *Handler) Confirm(c *gin.Context) {
	user, ok := h.requireManager(c)
	if !ok {
		return
	}
	var req confirmRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "session_id is required")
		return
	}
	ctx := c.Request.Context()
	s, err := h.stripe.GetCheckoutSession(ctx, req.SessionID)
	if err != nil || s.ClientReferenceID != user.OrgID.String() {
		// Another org's session looks exactly like a missing one.
		response.Fail(c, http.StatusNotFound, "CHECKOUT_NOT_FOUND", "Checkout session not found")
		return
	}
	if s.Status != "complete" || s.Subscription == nil {
		response.Fail(c, http.StatusConflict, "CHECKOUT_NOT_COMPLETE", "That checkout hasn't been completed")
		return
	}
	if _, _, err := h.syncer.SyncSubscription(ctx, s.Subscription.ID, true); err != nil {
		slog.Error("billing: confirming checkout", "error", err)
		failStripe(c, err)
		return
	}
	view, err := h.subscriptionView(ctx, user)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load subscription")
		return
	}
	response.OK(c, http.StatusOK, "", view)
}

// Portal opens Stripe's hosted billing portal, where the founder updates
// their card, sees invoices, or cancels.
func (h *Handler) Portal(c *gin.Context) {
	user, ok := h.requireManager(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	org, err := dbgen.New(h.systemPool).GetOrgBilling(ctx, user.OrgID)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load subscription")
		return
	}
	if deref(org.StripeCustomerID) == "" {
		response.Fail(c, http.StatusConflict, "NO_BILLING_ACCOUNT", "Choose a plan first — there's no billing account to manage yet")
		return
	}
	portalURL, err := h.stripe.CreatePortalSession(ctx, *org.StripeCustomerID, h.frontendURL+"/settings/billing")
	if err != nil {
		failStripe(c, err)
		return
	}
	response.OK(c, http.StatusOK, "", gin.H{"url": portalURL})
}

// requireManager gates the routes that change billing: owner/admin of an
// org that bills for itself, with Stripe configured.
func (h *Handler) requireManager(c *gin.Context) (authctx.User, bool) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return user, false
	}
	switch {
	case user.OrganizationType == "client_workspace":
		response.Fail(c, http.StatusForbidden, "BILLING_MANAGED_BY_PRACTICE", "This workspace is billed through its practice")
		return user, false
	case !user.IsOwnerOrAdmin():
		response.Fail(c, http.StatusForbidden, "NOT_AUTHORIZED", "Only an owner or admin can manage billing")
		return user, false
	case h.stripe == nil:
		response.Fail(c, http.StatusServiceUnavailable, "BILLING_NOT_CONFIGURED", "Billing isn't configured on this server")
		return user, false
	}
	return user, true
}

func failStripe(c *gin.Context, err error) {
	if errors.Is(err, corebilling.ErrNotSetUp) {
		response.Fail(c, http.StatusServiceUnavailable, "BILLING_NOT_CONFIGURED", "Billing plans aren't set up in Stripe yet")
		return
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		slog.Error("billing: stripe unreachable", "error", err)
	} else {
		slog.Error("billing: stripe call failed", "error", err)
	}
	response.Fail(c, http.StatusBadGateway, "STRIPE_ERROR", "Couldn't reach the payment provider — please try again")
}

func timeString(t pgtype.Timestamptz) *string {
	if !t.Valid {
		return nil
	}
	s := t.Time.UTC().Format(time.RFC3339)
	return &s
}

func plural(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

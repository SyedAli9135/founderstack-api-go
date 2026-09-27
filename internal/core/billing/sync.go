package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v86"

	"github.com/founderstack/api/internal/core/notify"
	"github.com/founderstack/api/internal/db/dbgen"
)

// ErrUnknownCustomer means no org owns the Stripe customer an event is
// about — usually a customer created by hand in the Stripe dashboard.
var ErrUnknownCustomer = errors.New("billing: no organization for this Stripe customer")

// State is what an org row records about its subscription.
type State struct {
	Plan              Plan
	Status            string
	SubscriptionID    string
	PeriodEnd         time.Time
	TrialEnd          time.Time
	CancelAtPeriodEnd bool
}

// StateFromSubscription derives the org's plan from a Stripe subscription.
// A subscription that no longer grants a paid plan (canceled, unpaid,
// incomplete) drops the org to the default plan's limits.
func StateFromSubscription(sub *stripe.Subscription) State {
	st := State{Plan: DefaultPlan, Status: string(sub.Status), SubscriptionID: sub.ID, CancelAtPeriodEnd: sub.CancelAtPeriodEnd}
	if item := PlanItem(sub); item != nil && IsLive(st.Status) {
		st.Plan, _ = PlanByLookupKey(item.Price.LookupKey)
	}
	if sub.Items != nil {
		for _, it := range sub.Items.Data {
			if end := time.Unix(it.CurrentPeriodEnd, 0); it.CurrentPeriodEnd > 0 && end.After(st.PeriodEnd) {
				st.PeriodEnd = end
			}
		}
	}
	if sub.TrialEnd > 0 {
		st.TrialEnd = time.Unix(sub.TrialEnd, 0)
	}
	// A cancel scheduled for a date is the same promise to the founder as
	// cancel_at_period_end.
	if sub.CancelAt > 0 {
		st.CancelAtPeriodEnd = true
	}
	return st
}

// Syncer writes Stripe subscription state onto organizations. It always
// works from a subscription freshly fetched from Stripe, never from a
// webhook payload, so out-of-order or redelivered events converge on
// Stripe's current truth.
type Syncer struct {
	systemPool  *pgxpool.Pool
	api         Stripe
	email       notify.EmailSender
	frontendURL string
}

func NewSyncer(systemPool *pgxpool.Pool, api Stripe, email notify.EmailSender, frontendURL string) *Syncer {
	return &Syncer{systemPool: systemPool, api: api, email: email, frontendURL: frontendURL}
}

// OrgForCustomer resolves the org that owns a Stripe customer.
func (s *Syncer) OrgForCustomer(ctx context.Context, customerID string) (pgtype.UUID, error) {
	id, err := dbgen.New(s.systemPool).GetOrgIDByStripeCustomer(ctx, &customerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, ErrUnknownCustomer
	}
	return id, err
}

// SyncSubscription fetches subID from Stripe and applies it to the org
// that owns its customer. adopt makes it the org's current subscription
// even if another one is live (a completed Checkout); without it a
// subscription only updates the org it already belongs to, or one with no
// live subscription, so a stale event for an old subscription can never
// overwrite the current plan. It returns the applied state, or nil when
// the subscription isn't the org's current one.
func (s *Syncer) SyncSubscription(ctx context.Context, subID string, adopt bool) (pgtype.UUID, *State, error) {
	sub, err := s.api.GetSubscription(ctx, subID)
	if err != nil {
		return pgtype.UUID{}, nil, fmt.Errorf("billing: fetching subscription: %w", err)
	}
	if sub.Customer == nil {
		return pgtype.UUID{}, nil, errors.New("billing: subscription has no customer")
	}
	orgID, err := s.OrgForCustomer(ctx, sub.Customer.ID)
	if err != nil {
		return pgtype.UUID{}, nil, err
	}
	st, err := s.Apply(ctx, orgID, sub, adopt)
	return orgID, st, err
}

// Apply writes sub onto orgID under the rules SyncSubscription describes.
func (s *Syncer) Apply(ctx context.Context, orgID pgtype.UUID, sub *stripe.Subscription, adopt bool) (*State, error) {
	st := StateFromSubscription(sub)
	var replaced string
	var applied bool
	err := pgx.BeginFunc(ctx, s.systemPool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		cur, err := q.LockOrgSubscription(ctx, orgID)
		if err != nil {
			return err
		}
		curID := deref(cur.StripeSubscriptionID)
		curLive := curID != "" && IsLive(deref(cur.SubscriptionStatus))
		switch {
		case curID == "" || curID == sub.ID:
		case adopt:
			if curLive {
				replaced = curID
			}
		case !curLive && IsLive(st.Status):
		default:
			return nil
		}
		applied = true
		if err := q.ApplySubscriptionState(ctx, stateParams(orgID, st)); err != nil {
			return err
		}
		return q.InheritPracticePlan(ctx, dbgen.InheritPracticePlanParams{PracticeID: orgID})
	})
	if err != nil {
		return nil, err
	}
	// Two paid Checkouts (say, from two tabs) would otherwise leave the
	// founder billed twice; the newer one wins and the older is refunded
	// pro rata.
	if replaced != "" {
		if err := s.api.CancelSubscription(ctx, replaced); err != nil {
			slog.Error("billing: canceling replaced subscription", "subscription", replaced, "err", err)
		}
	}
	if !applied {
		return nil, nil
	}
	// A plan change moves the practice to another tier's included count and
	// extra-workspace price. Best effort: the plan itself is already saved,
	// and the daily job retries a failed reconcile.
	if IsLive(st.Status) {
		if err := s.SyncWorkspaceUsage(ctx, orgID); err != nil {
			slog.Error("billing: syncing workspace usage after plan change", "org_id", orgID.String(), "err", err)
		}
	}
	return &st, nil
}

func stateParams(orgID pgtype.UUID, st State) dbgen.ApplySubscriptionStateParams {
	p := st.Plan
	return dbgen.ApplySubscriptionStateParams{
		ID: orgID, PlanTier: &p.Tier, SubscriptionStatus: &st.Status, StripeSubscriptionID: &st.SubscriptionID,
		CurrentPeriodEnd: ts(st.PeriodEnd), CancelAtPeriodEnd: st.CancelAtPeriodEnd, TrialEndsAt: ts(st.TrialEnd),
		MaxAgents: &p.MaxAgents, MaxWorkflows: &p.MaxWorkflows, MaxRagStorageGb: &p.MaxStorageGB,
		MaxMcpIntegrations: &p.MaxIntegrations, IncludedClientWorkspaces: p.IncludedClientWorkspaces,
		MaxClientWorkspaces: p.MaxClientWorkspaces,
	}
}

// NotifyPaymentFailed emails the org's owners and admins that a payment
// failed. Best effort: a mail failure is logged, never retried by
// failing the webhook, or Stripe's redelivery would resend to everyone
// who did get it.
func (s *Syncer) NotifyPaymentFailed(ctx context.Context, orgID pgtype.UUID, amountDue int64, hostedInvoiceURL string) {
	q := dbgen.New(s.systemPool)
	org, err := q.GetOrgBilling(ctx, orgID)
	if err != nil {
		slog.Error("billing: payment-failed email: loading org", "err", err)
		return
	}
	contacts, err := q.ListOrgBillingContacts(ctx, orgID)
	if err != nil {
		slog.Error("billing: payment-failed email: loading contacts", "err", err)
		return
	}
	billingURL := s.frontendURL + "/settings/billing"
	subject := "Payment failed — update your billing to keep your agents running"
	amount := fmt.Sprintf("$%.2f", float64(amountDue)/100)
	text := fmt.Sprintf("We couldn't charge %s for %s's FounderStack subscription.\n\n"+
		"Stripe will retry over the next few days. To avoid your agents being paused, update your payment method:\n%s\n",
		amount, org.Name, billingURL)
	if hostedInvoiceURL != "" {
		text += "\nYou can also pay the invoice directly: " + hostedInvoiceURL + "\n"
	}
	html := "<p>" + htmlLines(text) + "</p>"
	for _, c := range contacts {
		if err := s.email.Send(ctx, c.Email, subject, text, html); err != nil {
			slog.Error("billing: payment-failed email", "to_org", org.Name, "err", err)
		}
	}
	slog.Info("billing: payment-failed notice sent", "org", org.Name, "recipients", len(contacts))
}

// NotifyUpcomingOverage warns a practice's owners and admins, ahead of a
// renewal, that the invoice includes client workspaces beyond their plan.
// Nothing is sent when there are none.
func (s *Syncer) NotifyUpcomingOverage(ctx context.Context, orgID pgtype.UUID) {
	q := dbgen.New(s.systemPool)
	org, err := q.GetOrgBilling(ctx, orgID)
	if err != nil || org.OrganizationType != "practice" {
		return
	}
	plan, ok := PlanByTier(deref(org.PlanTier))
	extra := plan.ExtraWorkspaces(org.ActiveClientWorkspaces)
	if !ok || extra == 0 {
		return
	}
	contacts, err := q.ListOrgBillingContacts(ctx, orgID)
	if err != nil {
		slog.Error("billing: upcoming-overage email: loading contacts", "err", err)
		return
	}
	subject := fmt.Sprintf("Your next FounderStack invoice includes %d extra client workspace%s", extra, plural(extra))
	text := fmt.Sprintf("%s has %d active client workspaces; your %s plan includes %d.\n\n"+
		"Your next invoice includes %d additional workspace%s at $%d/month each ($%d), on top of the $%d plan.\n\n"+
		"Remove workspaces you no longer need before the renewal to stop paying for them:\n%s/practice\n",
		org.Name, org.ActiveClientWorkspaces, plan.Name, plan.IncludedClientWorkspaces,
		extra, plural(extra), plan.ExtraWorkspaceUSD, extra*plan.ExtraWorkspaceUSD, plan.MonthlyPriceUSD, s.frontendURL)
	html := "<p>" + htmlLines(text) + "</p>"
	for _, c := range contacts {
		if err := s.email.Send(ctx, c.Email, subject, text, html); err != nil {
			slog.Error("billing: upcoming-overage email", "org", org.Name, "err", err)
		}
	}
	slog.Info("billing: upcoming-overage notice sent", "org", org.Name, "extra_workspaces", extra, "recipients", len(contacts))
}

func plural(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func htmlLines(s string) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch r {
		case '<':
			out = append(out, "&lt;"...)
		case '>':
			out = append(out, "&gt;"...)
		case '&':
			out = append(out, "&amp;"...)
		case '\n':
			out = append(out, "<br>"...)
		default:
			out = append(out, string(r)...)
		}
	}
	return string(out)
}

func ts(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"

	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/core/billing"
	"github.com/founderstack/api/internal/db/dbgen"
)

// Stripe signs well under this; anything larger isn't a real event.
const maxStripePayload = 1 << 20

// StripeHandler receives FounderStack's own platform-billing events
// (workflow 15) — not the Stripe integration agents use, which is a
// per-org OAuth connection with no webhook of its own.
type StripeHandler struct {
	systemPool *pgxpool.Pool
	syncer     *billing.Syncer
	secret     string
}

// syncer is nil when STRIPE_SECRET_KEY is unset; secret is
// STRIPE_WEBHOOK_SECRET.
func NewStripeHandler(systemPool *pgxpool.Pool, syncer *billing.Syncer, webhookSecret string) *StripeHandler {
	return &StripeHandler{systemPool: systemPool, syncer: syncer, secret: webhookSecret}
}

func (h *StripeHandler) Register(rg *gin.RouterGroup) {
	rg.POST("/stripe", h.Handle)
}

func (h *StripeHandler) Handle(c *gin.Context) {
	if h.secret == "" || h.syncer == nil {
		slog.Error("stripe webhook received but STRIPE_SECRET_KEY/STRIPE_WEBHOOK_SECRET is not set")
		response.Fail(c, http.StatusServiceUnavailable, "BILLING_NOT_CONFIGURED", "Billing is not configured")
		return
	}
	body, err := readLimited(c, maxStripePayload)
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_BODY", "Could not read request body")
		return
	}
	// The endpoint's API version is whatever was chosen in the Stripe
	// dashboard, which needn't match stripe-go's pinned one. That's safe to
	// ignore: only IDs are read from the payload, and every object is then
	// re-fetched through stripe-go at its own version.
	evt, err := webhook.ConstructEventWithOptions(body, c.GetHeader("Stripe-Signature"), h.secret,
		webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		slog.Warn("stripe webhook signature verification failed", "error", err, "request_id", response.RequestID(c))
		response.Fail(c, http.StatusBadRequest, "INVALID_SIGNATURE", "Invalid signature")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*handlerTimeout)
	defer cancel()

	// The event row is inserted in a transaction held open while the event
	// is handled: a concurrent redelivery blocks on it, then sees the
	// conflict and skips; a failed handling rolls it back so Stripe's retry
	// runs it again.
	var duplicate bool
	err = pgx.BeginFunc(ctx, h.systemPool, func(tx pgx.Tx) error {
		if _, err := dbgen.New(tx).RecordStripeEvent(ctx, dbgen.RecordStripeEventParams{ID: evt.ID, Type: string(evt.Type)}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				duplicate = true
				return nil
			}
			return err
		}
		return h.dispatch(ctx, evt)
	})
	switch {
	case errors.Is(err, billing.ErrUnknownCustomer):
		// Not ours to act on (a customer made by hand in the dashboard, or
		// another environment sharing the Stripe account). Retrying can't
		// fix it, so acknowledge.
		slog.Warn("stripe webhook for unknown customer", "type", evt.Type, "event", evt.ID)
		response.OK(c, http.StatusOK, "Ignored", gin.H{"ignored": true})
		return
	case err != nil:
		slog.Error("stripe webhook handling failed", "type", evt.Type, "event", evt.ID, "error", err)
		response.Fail(c, http.StatusInternalServerError, "WEBHOOK_FAILED", "Could not process event")
		return
	}
	slog.Info("stripe webhook processed", "type", evt.Type, "event", evt.ID, "duplicate", duplicate)
	response.OK(c, http.StatusOK, "OK", gin.H{"received": true})
}

func (h *StripeHandler) dispatch(ctx context.Context, evt stripe.Event) error {
	switch evt.Type {
	case "checkout.session.completed":
		var s stripe.CheckoutSession
		if err := json.Unmarshal(evt.Data.Raw, &s); err != nil {
			return err
		}
		if s.Mode != stripe.CheckoutSessionModeSubscription || s.Subscription == nil {
			return nil
		}
		// A completed Checkout is the founder's explicit choice of plan, so
		// its subscription becomes current even if another one is live.
		_, _, err := h.syncer.SyncSubscription(ctx, s.Subscription.ID, true)
		return err

	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
		var sub stripe.Subscription
		if err := json.Unmarshal(evt.Data.Raw, &sub); err != nil {
			return err
		}
		_, _, err := h.syncer.SyncSubscription(ctx, sub.ID, false)
		return err

	case "invoice.paid", "invoice.payment_failed":
		var inv stripe.Invoice
		if err := json.Unmarshal(evt.Data.Raw, &inv); err != nil {
			return err
		}
		subID := invoiceSubscriptionID(&inv)
		if subID == "" {
			return nil
		}
		orgID, st, err := h.syncer.SyncSubscription(ctx, subID, false)
		if err != nil {
			return err
		}
		// Only warn about the org's current subscription — a failure on a
		// replaced one isn't something the founder needs to act on.
		if evt.Type == "invoice.payment_failed" && st != nil {
			h.syncer.NotifyPaymentFailed(ctx, orgID, inv.AmountDue, inv.HostedInvoiceURL)
		}
		return nil
	}
	return nil
}

func invoiceSubscriptionID(inv *stripe.Invoice) string {
	if inv.Parent != nil && inv.Parent.SubscriptionDetails != nil && inv.Parent.SubscriptionDetails.Subscription != nil {
		return inv.Parent.SubscriptionDetails.Subscription.ID
	}
	return ""
}

func readLimited(c *gin.Context, limit int64) ([]byte, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	return c.GetRawData()
}

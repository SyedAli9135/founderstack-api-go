package servers

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	mcp "github.com/founderstack/api/internal/core/mcp"
)

// stripeAPIBase is a var, not a const, purely so stripe_test.go can point
// it at a fake httptest server instead of the real Stripe API — same
// "test the request-shape logic, not a live third-party dependency"
// reasoning as internal/core/llm/verify.go's geminiModelsURL.
var stripeAPIBase = "https://api.stripe.com/v1"

// NewStripeServer builds the Stripe MCP tool server — the "Founder's
// Core 5" finance tool (WORKFLOW_PLAN_GO.md workflow 5). Auth is Basic
// (secret key as username, empty password), matching Stripe's own
// convention and internal/core/integrations/providers/stripe.go's
// ValidateKey. Plain REST calls, not stripe-go — same "one dependency
// per real need" reasoning as everywhere else BYOK/integrations touches
// a third-party API in this codebase.
func NewStripeServer() *gomcp.Server {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "stripe", Version: "1.0.0"}, nil)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "list_subscriptions",
		Description: "List active Stripe subscriptions for the connected account.",
		Annotations: mcp.ReadOnly(),
	}, stripeListSubscriptions)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "create_invoice",
		Description: "Create a draft Stripe invoice for a customer. Set finalize=true to also finalize it immediately.",
		// Reversible, not financial-destructive: this creates/finalizes a
		// bill but doesn't itself move money (no auto-charge unless the
		// org's own Stripe collection settings do that), and a draft/open
		// invoice can still be voided.
		Annotations: mcp.ReversibleWrite(),
	}, stripeCreateInvoice)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "refund_payment",
		Description: "Refund a Stripe payment intent, fully or for a specific amount (in the smallest currency unit, e.g. cents).",
		Annotations: mcp.DestructiveOrFinancial(),
	}, stripeRefundPayment)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "get_mrr",
		Description: "Estimate current Monthly Recurring Revenue from active subscriptions.",
		Annotations: mcp.ReadOnly(),
	}, stripeGetMRR)

	return server
}

type stripeListSubscriptionsInput struct {
	// Limit caps how many subscriptions to return (Stripe's own page
	// size cap is 100); default 20 keeps a typical tool-call response
	// small enough for a model to read without truncation.
	Limit int `json:"limit,omitempty" jsonschema:"Maximum subscriptions to return (default 20, max 100)"`
}

type stripeSubscriptionSummary struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	CustomerID string `json:"customer_id"`
}

type stripeListSubscriptionsOutput struct {
	Subscriptions []stripeSubscriptionSummary `json:"subscriptions"`
}

func stripeListSubscriptions(ctx context.Context, req *gomcp.CallToolRequest, in stripeListSubscriptionsInput) (*gomcp.CallToolResult, stripeListSubscriptionsOutput, error) {
	token, ok := mcp.TokenFromRequest(req)
	if !ok {
		return nil, stripeListSubscriptionsOutput{}, fmt.Errorf("stripe: no token in request metadata")
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	endpoint := fmt.Sprintf("%s/subscriptions?status=active&limit=%d", stripeAPIBase, limit)
	var resp stripeSubscriptionListResponse
	if err := doStripeForm(ctx, "GET", endpoint, token, "", nil, &resp); err != nil {
		return nil, stripeListSubscriptionsOutput{}, fmt.Errorf("stripe: list subscriptions: %w", err)
	}

	out := stripeListSubscriptionsOutput{Subscriptions: make([]stripeSubscriptionSummary, 0, len(resp.Data))}
	for _, sub := range resp.Data {
		out.Subscriptions = append(out.Subscriptions, stripeSubscriptionSummary{
			ID:         sub.ID,
			Status:     sub.Status,
			CustomerID: sub.Customer,
		})
	}
	return nil, out, nil
}

type stripeCreateInvoiceInput struct {
	CustomerID  string `json:"customer_id" jsonschema:"Stripe customer ID to invoice (e.g. cus_...)"`
	AmountCents int64  `json:"amount_cents" jsonschema:"Line item amount in the smallest currency unit (e.g. cents) — Stripe requires at least one pending invoice item before it will create an invoice at all"`
	Currency    string `json:"currency,omitempty" jsonschema:"Three-letter ISO currency code for the line item (default usd)"`
	Description string `json:"description,omitempty" jsonschema:"Optional line-item/invoice description"`
	Finalize    bool   `json:"finalize,omitempty" jsonschema:"If true, finalize the invoice immediately instead of leaving it as a draft"`
}

type stripeCreateInvoiceOutput struct {
	InvoiceID        string `json:"invoice_id"`
	Status           string `json:"status"`
	HostedInvoiceURL string `json:"hosted_invoice_url,omitempty"`
}

type stripeInvoiceItemResponse struct {
	ID string `json:"id"`
}

func stripeCreateInvoice(ctx context.Context, req *gomcp.CallToolRequest, in stripeCreateInvoiceInput) (*gomcp.CallToolResult, stripeCreateInvoiceOutput, error) {
	token, ok := mcp.TokenFromRequest(req)
	if !ok {
		return nil, stripeCreateInvoiceOutput{}, fmt.Errorf("stripe: no token in request metadata")
	}
	if in.CustomerID == "" {
		return nil, stripeCreateInvoiceOutput{}, fmt.Errorf("stripe: customer_id is required")
	}
	if in.AmountCents <= 0 {
		return nil, stripeCreateInvoiceOutput{}, fmt.Errorf("stripe: amount_cents must be greater than 0")
	}
	currency := in.Currency
	if currency == "" {
		currency = "usd"
	}
	// idempotencyKey, if any, protects this whole logical tool call from
	// double-executing on a retry — see doStripeForm's doc comment. The
	// invoice-item and finalize sub-steps each get their own derived key
	// (distinct Stripe writes, not covered by the create call's key).
	idempotencyKey, _ := mcp.IdempotencyKeyFromRequest(req)

	// Real Stripe behavior, confirmed live 2026-08-28 (not obvious from a
	// first read of the API docs): POST /v1/invoices errors with
	// "invoice_no_customer_line_items" unless the customer already has at
	// least one pending invoice item — an earlier version of this handler
	// skipped this step entirely and could never successfully create an
	// invoice for any customer. Create the line item first, then the
	// invoice picks up every pending item for the customer automatically.
	itemForm := url.Values{
		"customer": {in.CustomerID},
		"amount":   {strconv.FormatInt(in.AmountCents, 10)},
		"currency": {currency},
	}
	if in.Description != "" {
		itemForm.Set("description", in.Description)
	}
	itemKey := idempotencyKey
	if itemKey != "" {
		itemKey += "-item"
	}
	var item stripeInvoiceItemResponse
	if err := doStripeForm(ctx, "POST", stripeAPIBase+"/invoiceitems", token, itemKey, itemForm, &item); err != nil {
		return nil, stripeCreateInvoiceOutput{}, fmt.Errorf("stripe: create invoice item: %w", err)
	}

	form := url.Values{"customer": {in.CustomerID}}
	if in.Description != "" {
		form.Set("description", in.Description)
	}

	var invoice stripeInvoiceResponse
	if err := doStripeForm(ctx, "POST", stripeAPIBase+"/invoices", token, idempotencyKey, form, &invoice); err != nil {
		return nil, stripeCreateInvoiceOutput{}, fmt.Errorf("stripe: create invoice: %w", err)
	}

	if in.Finalize {
		var finalized stripeInvoiceResponse
		finalizeURL := fmt.Sprintf("%s/invoices/%s/finalize", stripeAPIBase, invoice.ID)
		finalizeKey := idempotencyKey
		if finalizeKey != "" {
			finalizeKey += "-finalize"
		}
		if err := doStripeForm(ctx, "POST", finalizeURL, token, finalizeKey, url.Values{}, &finalized); err != nil {
			return nil, stripeCreateInvoiceOutput{}, fmt.Errorf("stripe: finalize invoice %s: %w", invoice.ID, err)
		}
		invoice = finalized
	}

	return nil, stripeCreateInvoiceOutput{
		InvoiceID:        invoice.ID,
		Status:           invoice.Status,
		HostedInvoiceURL: invoice.HostedInvoiceURL,
	}, nil
}

type stripeRefundPaymentInput struct {
	PaymentIntentID string `json:"payment_intent_id" jsonschema:"Stripe PaymentIntent ID to refund (e.g. pi_...)"`
	// AmountCents refunds a partial amount when set; a full refund when
	// omitted/zero, matching Stripe's own "omit amount for a full refund"
	// convention.
	AmountCents int64 `json:"amount_cents,omitempty" jsonschema:"Amount to refund in the smallest currency unit (e.g. cents). Omit for a full refund."`
}

type stripeRefundPaymentOutput struct {
	RefundID string `json:"refund_id"`
	Status   string `json:"status"`
}

func stripeRefundPayment(ctx context.Context, req *gomcp.CallToolRequest, in stripeRefundPaymentInput) (*gomcp.CallToolResult, stripeRefundPaymentOutput, error) {
	token, ok := mcp.TokenFromRequest(req)
	if !ok {
		return nil, stripeRefundPaymentOutput{}, fmt.Errorf("stripe: no token in request metadata")
	}
	if in.PaymentIntentID == "" {
		return nil, stripeRefundPaymentOutput{}, fmt.Errorf("stripe: payment_intent_id is required")
	}
	idempotencyKey, _ := mcp.IdempotencyKeyFromRequest(req)

	form := url.Values{"payment_intent": {in.PaymentIntentID}}
	if in.AmountCents > 0 {
		form.Set("amount", strconv.FormatInt(in.AmountCents, 10))
	}

	var refund stripeRefundResponse
	if err := doStripeForm(ctx, "POST", stripeAPIBase+"/refunds", token, idempotencyKey, form, &refund); err != nil {
		return nil, stripeRefundPaymentOutput{}, fmt.Errorf("stripe: refund payment: %w", err)
	}

	return nil, stripeRefundPaymentOutput{RefundID: refund.ID, Status: refund.Status}, nil
}

type stripeGetMRRInput struct{}

type stripeGetMRROutput struct {
	// MRRCents is an estimate, not a billing-grade figure — see the
	// interval-normalization comment on monthlyNormalizedCents below.
	MRRCents            int64  `json:"mrr_cents"`
	Currency            string `json:"currency"`
	ActiveSubscriptions int    `json:"active_subscriptions"`
}

// maxMRRPages caps how many 100-subscription pages get(mrr) will fetch —
// 20 pages (2000 subscriptions) is far beyond what any solo-founder-scale
// account has today; the cap exists so a runaway account (or a bug in
// Stripe's has_more pagination) can't turn one tool call into an
// unbounded loop.
const maxMRRPages = 20

func stripeGetMRR(ctx context.Context, req *gomcp.CallToolRequest, _ stripeGetMRRInput) (*gomcp.CallToolResult, stripeGetMRROutput, error) {
	token, ok := mcp.TokenFromRequest(req)
	if !ok {
		return nil, stripeGetMRROutput{}, fmt.Errorf("stripe: no token in request metadata")
	}

	var totalCents int64
	var currency string
	var count int
	startingAfter := ""

	for page := 0; page < maxMRRPages; page++ {
		endpoint := fmt.Sprintf("%s/subscriptions?status=active&limit=100&expand[]=data.items.data.price", stripeAPIBase)
		if startingAfter != "" {
			endpoint += "&starting_after=" + startingAfter
		}

		var resp stripeSubscriptionListResponse
		if err := doStripeForm(ctx, "GET", endpoint, token, "", nil, &resp); err != nil {
			return nil, stripeGetMRROutput{}, fmt.Errorf("stripe: list subscriptions for MRR: %w", err)
		}

		for _, sub := range resp.Data {
			for _, item := range sub.Items.Data {
				price := item.Price
				if price.Currency != "" {
					currency = price.Currency
				}
				totalCents += monthlyNormalizedCents(price.UnitAmount, item.Quantity, price.Recurring.Interval, price.Recurring.IntervalCount)
			}
			count++
		}

		if !resp.HasMore || len(resp.Data) == 0 {
			break
		}
		startingAfter = resp.Data[len(resp.Data)-1].ID
	}

	return nil, stripeGetMRROutput{MRRCents: totalCents, Currency: currency, ActiveSubscriptions: count}, nil
}

// monthlyNormalizedCents converts one subscription item's price into a
// monthly-equivalent amount. Months and years divide/multiply exactly;
// weeks and days use the standard average-per-month approximation
// (365.25/12 days, 52.18/12 weeks) every calendar-normalized billing
// estimate makes, since calendar months aren't a fixed length — this is
// an estimate for planning purposes, not a billing-grade reconciliation
// figure.
func monthlyNormalizedCents(unitAmount, quantity int64, interval string, intervalCount int64) int64 {
	if intervalCount <= 0 {
		intervalCount = 1
	}
	amount := float64(unitAmount * quantity)
	switch interval {
	case "year":
		amount = amount / (12 * float64(intervalCount))
	case "week":
		amount = amount * (52.18 / 12) / float64(intervalCount)
	case "day":
		amount = amount * (365.25 / 12) / float64(intervalCount)
	default: // "month"
		amount = amount / float64(intervalCount)
	}
	return int64(amount)
}

// --- Stripe API response shapes (only the fields these tools use) ---

type stripeSubscriptionListResponse struct {
	Data    []stripeSubscription `json:"data"`
	HasMore bool                 `json:"has_more"`
}

type stripeSubscription struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Customer string `json:"customer"`
	Items    struct {
		Data []stripeSubscriptionItem `json:"data"`
	} `json:"items"`
}

type stripeSubscriptionItem struct {
	Quantity int64 `json:"quantity"`
	Price    struct {
		UnitAmount int64  `json:"unit_amount"`
		Currency   string `json:"currency"`
		Recurring  struct {
			Interval      string `json:"interval"`
			IntervalCount int64  `json:"interval_count"`
		} `json:"recurring"`
	} `json:"price"`
}

type stripeInvoiceResponse struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	HostedInvoiceURL string `json:"hosted_invoice_url"`
}

type stripeRefundResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

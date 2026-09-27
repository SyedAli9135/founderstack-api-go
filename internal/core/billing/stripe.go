package billing

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/stripe/stripe-go/v86"
)

// ErrNotSetUp means the Stripe account is missing the products, prices or
// portal configuration cmd/stripe-setup creates.
var ErrNotSetUp = errors.New("billing: Stripe products are not set up — run `make stripe-setup`")

// PortalConfigMetadataKey marks the billing portal configuration
// cmd/stripe-setup creates, so the API can find it without an env var.
const PortalConfigMetadataKey = "founderstack_portal"

// CheckoutParams describes one Checkout session for a plan.
type CheckoutParams struct {
	OrgID      string
	CustomerID string
	Plan       Plan
	SuccessURL string
	CancelURL  string
}

// Stripe is the subset of the Stripe API billing uses. It's an interface
// so handler and webhook tests can run against a fake instead of the
// network.
type Stripe interface {
	CreateCustomer(ctx context.Context, orgID, name, email string) (string, error)
	CreateCheckoutSession(ctx context.Context, p CheckoutParams) (string, error)
	GetCheckoutSession(ctx context.Context, id string) (*stripe.CheckoutSession, error)
	GetSubscription(ctx context.Context, id string) (*stripe.Subscription, error)
	ChangePlan(ctx context.Context, sub *stripe.Subscription, plan Plan) (*stripe.Subscription, error)
	CancelSubscription(ctx context.Context, id string) error
	CreatePortalSession(ctx context.Context, customerID, returnURL string) (string, error)
}

// StripeClient is the real Stripe implementation. Price and portal
// configuration IDs are resolved on first use and cached.
type StripeClient struct {
	sc *stripe.Client

	mu           sync.Mutex
	prices       map[string]string
	portalConfig string
}

func NewStripeClient(secretKey string) *StripeClient {
	return &StripeClient{sc: stripe.NewClient(secretKey), prices: map[string]string{}}
}

func (c *StripeClient) priceID(ctx context.Context, lookupKey string) (string, error) {
	c.mu.Lock()
	id, ok := c.prices[lookupKey]
	c.mu.Unlock()
	if ok {
		return id, nil
	}
	params := &stripe.PriceListParams{Active: stripe.Bool(true), LookupKeys: []*string{stripe.String(lookupKey)}}
	for p, err := range c.sc.V1Prices.List(ctx, params).All(ctx) {
		if err != nil {
			return "", fmt.Errorf("billing: looking up price %s: %w", lookupKey, err)
		}
		c.mu.Lock()
		c.prices[lookupKey] = p.ID
		c.mu.Unlock()
		return p.ID, nil
	}
	return "", ErrNotSetUp
}

// CreateCustomer is idempotent per org: two concurrent upgrade clicks get
// the same Stripe customer back rather than two.
func (c *StripeClient) CreateCustomer(ctx context.Context, orgID, name, email string) (string, error) {
	params := &stripe.CustomerCreateParams{Name: stripe.String(name), Metadata: map[string]string{"org_id": orgID}}
	if email != "" {
		params.Email = stripe.String(email)
	}
	params.SetIdempotencyKey("fs-customer-" + orgID)
	cust, err := c.sc.V1Customers.Create(ctx, params)
	if err != nil {
		return "", err
	}
	return cust.ID, nil
}

func (c *StripeClient) CreateCheckoutSession(ctx context.Context, p CheckoutParams) (string, error) {
	priceID, err := c.priceID(ctx, p.Plan.LookupKey)
	if err != nil {
		return "", err
	}
	s, err := c.sc.V1CheckoutSessions.Create(ctx, &stripe.CheckoutSessionCreateParams{
		Mode:              stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		Customer:          stripe.String(p.CustomerID),
		ClientReferenceID: stripe.String(p.OrgID),
		LineItems:         []*stripe.CheckoutSessionCreateLineItemParams{{Price: stripe.String(priceID), Quantity: stripe.Int64(1)}},
		SuccessURL:        stripe.String(p.SuccessURL),
		CancelURL:         stripe.String(p.CancelURL),
		SubscriptionData: &stripe.CheckoutSessionCreateSubscriptionDataParams{
			Metadata: map[string]string{"org_id": p.OrgID, "tier": p.Plan.Tier},
		},
	})
	if err != nil {
		return "", err
	}
	return s.URL, nil
}

func (c *StripeClient) GetCheckoutSession(ctx context.Context, id string) (*stripe.CheckoutSession, error) {
	return c.sc.V1CheckoutSessions.Retrieve(ctx, id, nil)
}

func (c *StripeClient) GetSubscription(ctx context.Context, id string) (*stripe.Subscription, error) {
	params := &stripe.SubscriptionRetrieveParams{}
	params.AddExpand("items.data.price")
	return c.sc.V1Subscriptions.Retrieve(ctx, id, params)
}

// ChangePlan swaps the subscription's plan item to plan's price. Stripe
// prorates the difference onto the next invoice. Choosing a plan also
// undoes a cancellation scheduled from the billing portal — picking a
// plan means the founder wants to stay.
func (c *StripeClient) ChangePlan(ctx context.Context, sub *stripe.Subscription, plan Plan) (*stripe.Subscription, error) {
	item := PlanItem(sub)
	if item == nil {
		return nil, errors.New("billing: subscription has no plan item")
	}
	priceID, err := c.priceID(ctx, plan.LookupKey)
	if err != nil {
		return nil, err
	}
	params := &stripe.SubscriptionUpdateParams{
		Items:             []*stripe.SubscriptionUpdateItemParams{{ID: stripe.String(item.ID), Price: stripe.String(priceID)}},
		ProrationBehavior: stripe.String("create_prorations"),
		CancelAtPeriodEnd: stripe.Bool(false),
		Metadata:          map[string]string{"tier": plan.Tier},
	}
	params.AddExpand("items.data.price")
	return c.sc.V1Subscriptions.Update(ctx, sub.ID, params)
}

// CancelSubscription ends a subscription now, crediting the unused time.
func (c *StripeClient) CancelSubscription(ctx context.Context, id string) error {
	_, err := c.sc.V1Subscriptions.Cancel(ctx, id, &stripe.SubscriptionCancelParams{Prorate: stripe.Bool(true)})
	return err
}

func (c *StripeClient) CreatePortalSession(ctx context.Context, customerID, returnURL string) (string, error) {
	cfg, err := c.portalConfigID(ctx)
	if err != nil {
		return "", err
	}
	s, err := c.sc.V1BillingPortalSessions.Create(ctx, &stripe.BillingPortalSessionCreateParams{
		Customer: stripe.String(customerID), ReturnURL: stripe.String(returnURL), Configuration: stripe.String(cfg),
	})
	if err != nil {
		return "", err
	}
	return s.URL, nil
}

func (c *StripeClient) portalConfigID(ctx context.Context) (string, error) {
	c.mu.Lock()
	id := c.portalConfig
	c.mu.Unlock()
	if id != "" {
		return id, nil
	}
	params := &stripe.BillingPortalConfigurationListParams{Active: stripe.Bool(true)}
	for cfg, err := range c.sc.V1BillingPortalConfigurations.List(ctx, params).All(ctx) {
		if err != nil {
			return "", err
		}
		if cfg.Metadata[PortalConfigMetadataKey] == "true" {
			c.mu.Lock()
			c.portalConfig = cfg.ID
			c.mu.Unlock()
			return cfg.ID, nil
		}
	}
	return "", ErrNotSetUp
}

// PlanItem is the subscription item carrying the plan's price (as opposed
// to any add-on item).
func PlanItem(sub *stripe.Subscription) *stripe.SubscriptionItem {
	if sub == nil || sub.Items == nil {
		return nil
	}
	for _, it := range sub.Items.Data {
		if it.Price != nil {
			if _, ok := PlanByLookupKey(it.Price.LookupKey); ok {
				return it
			}
		}
	}
	return nil
}

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"github.com/stripe/stripe-go/v86"

	"github.com/founderstack/api/internal/core/billing"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "stripe-setup:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	_ = godotenv.Load()
	key := os.Getenv("STRIPE_SECRET_KEY")
	switch {
	case key == "":
		return fmt.Errorf("STRIPE_SECRET_KEY is not set")
	case !strings.HasPrefix(key, "sk_test_") && os.Getenv("STRIPE_SETUP_ALLOW_LIVE") != "true":
		return fmt.Errorf("refusing a non-test key; set STRIPE_SETUP_ALLOW_LIVE=true to set up a live account")
	}
	sc := stripe.NewClient(key)

	for _, p := range billing.Plans {
		prices := []price{{label: p.Tier, product: "FounderStack " + p.Name, lookupKey: p.LookupKey, usd: p.MonthlyPriceUSD, tier: p.Tier}}
		//each tier's "additional client workspace", billed per unit.
		if p.ExtraWorkspaceLookupKey != "" {
			prices = append(prices, price{
				label: p.Tier + "+ws", product: "FounderStack additional client workspace (" + p.Name + ")",
				lookupKey: p.ExtraWorkspaceLookupKey, usd: p.ExtraWorkspaceUSD, tier: p.Tier,
			})
		}
		for _, pr := range prices {
			if err := ensurePrice(ctx, sc, pr); err != nil {
				return fmt.Errorf("%s: %w", pr.lookupKey, err)
			}
		}
	}
	return ensurePortal(ctx, sc)
}

type price struct {
	label, product, lookupKey, tier string
	usd                             int64
}

func ensurePrice(ctx context.Context, sc *stripe.Client, p price) error {
	want := p.usd * 100
	var productID string
	params := &stripe.PriceListParams{Active: stripe.Bool(true), LookupKeys: []*string{stripe.String(p.lookupKey)}}
	for existing, err := range sc.V1Prices.List(ctx, params).All(ctx) {
		if err != nil {
			return err
		}
		if existing.UnitAmount == want && existing.Recurring != nil && existing.Recurring.Interval == "month" {
			fmt.Printf("  %-10s ok       %s ($%d/mo)\n", p.label, existing.ID, p.usd)
			return nil
		}
		productID = existing.Product.ID
	}

	if productID == "" {
		prod, err := sc.V1Products.Create(ctx, &stripe.ProductCreateParams{
			Name:     stripe.String(p.product),
			Metadata: map[string]string{"tier": p.tier},
		})
		if err != nil {
			return err
		}
		productID = prod.ID
	}
	created, err := sc.V1Prices.Create(ctx, &stripe.PriceCreateParams{
		Product:           stripe.String(productID),
		Currency:          stripe.String("usd"),
		UnitAmount:        stripe.Int64(want),
		Recurring:         &stripe.PriceCreateRecurringParams{Interval: stripe.String("month")},
		LookupKey:         stripe.String(p.lookupKey),
		TransferLookupKey: stripe.Bool(true),
		Nickname:          stripe.String(p.product + " monthly"),
		Metadata:          map[string]string{"tier": p.tier},
	})
	if err != nil {
		return err
	}
	fmt.Printf("  %-10s created  %s ($%d/mo)\n", p.label, created.ID, p.usd)
	return nil
}

// ensurePortal creates the customer portal configuration the billing page
// links to: update card, see invoices, cancel at period end. Plan changes
// stay in-app, where limits and warnings are shown.
func ensurePortal(ctx context.Context, sc *stripe.Client) error {
	for cfg, err := range sc.V1BillingPortalConfigurations.List(ctx, &stripe.BillingPortalConfigurationListParams{Active: stripe.Bool(true)}).All(ctx) {
		if err != nil {
			return err
		}
		if cfg.Metadata[billing.PortalConfigMetadataKey] == "true" {
			fmt.Printf("  %-10s ok       %s\n", "portal", cfg.ID)
			return nil
		}
	}
	cfg, err := sc.V1BillingPortalConfigurations.Create(ctx, &stripe.BillingPortalConfigurationCreateParams{
		Name: stripe.String("FounderStack"),
		BusinessProfile: &stripe.BillingPortalConfigurationCreateBusinessProfileParams{
			Headline: stripe.String("FounderStack — manage your subscription"),
		},
		Features: &stripe.BillingPortalConfigurationCreateFeaturesParams{
			PaymentMethodUpdate: &stripe.BillingPortalConfigurationCreateFeaturesPaymentMethodUpdateParams{Enabled: stripe.Bool(true)},
			InvoiceHistory:      &stripe.BillingPortalConfigurationCreateFeaturesInvoiceHistoryParams{Enabled: stripe.Bool(true)},
			CustomerUpdate: &stripe.BillingPortalConfigurationCreateFeaturesCustomerUpdateParams{
				Enabled: stripe.Bool(true), AllowedUpdates: stripe.StringSlice([]string{"email", "address", "tax_id"}),
			},
			SubscriptionCancel: &stripe.BillingPortalConfigurationCreateFeaturesSubscriptionCancelParams{
				Enabled: stripe.Bool(true), Mode: stripe.String("at_period_end"),
			},
		},
		Metadata: map[string]string{billing.PortalConfigMetadataKey: "true"},
	})
	if err != nil {
		return err
	}
	fmt.Printf("  %-10s created  %s\n", "portal", cfg.ID)
	return nil
}

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
		if err := ensurePrice(ctx, sc, p); err != nil {
			return fmt.Errorf("%s: %w", p.Tier, err)
		}
	}
	return ensurePortal(ctx, sc)
}

func ensurePrice(ctx context.Context, sc *stripe.Client, p billing.Plan) error {
	want := p.MonthlyPriceUSD * 100
	var productID string
	params := &stripe.PriceListParams{Active: stripe.Bool(true), LookupKeys: []*string{stripe.String(p.LookupKey)}}
	for price, err := range sc.V1Prices.List(ctx, params).All(ctx) {
		if err != nil {
			return err
		}
		if price.UnitAmount == want && price.Recurring != nil && price.Recurring.Interval == "month" {
			fmt.Printf("  %-8s ok       %s ($%d/mo)\n", p.Tier, price.ID, p.MonthlyPriceUSD)
			return nil
		}
		productID = price.Product.ID
	}

	if productID == "" {
		prod, err := sc.V1Products.Create(ctx, &stripe.ProductCreateParams{
			Name:     stripe.String("FounderStack " + p.Name),
			Metadata: map[string]string{"tier": p.Tier},
		})
		if err != nil {
			return err
		}
		productID = prod.ID
	}
	price, err := sc.V1Prices.Create(ctx, &stripe.PriceCreateParams{
		Product:           stripe.String(productID),
		Currency:          stripe.String("usd"),
		UnitAmount:        stripe.Int64(want),
		Recurring:         &stripe.PriceCreateRecurringParams{Interval: stripe.String("month")},
		LookupKey:         stripe.String(p.LookupKey),
		TransferLookupKey: stripe.Bool(true),
		Nickname:          stripe.String(p.Name + " monthly"),
		Metadata:          map[string]string{"tier": p.Tier},
	})
	if err != nil {
		return err
	}
	fmt.Printf("  %-8s created  %s ($%d/mo)\n", p.Tier, price.ID, p.MonthlyPriceUSD)
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
			fmt.Printf("  portal   ok       %s\n", cfg.ID)
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
	fmt.Printf("  portal   created  %s\n", cfg.ID)
	return nil
}

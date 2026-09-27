// Package billingtest provides an in-memory billing.Stripe for tests.
package billingtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/stripe/stripe-go/v86"

	"github.com/founderstack/api/internal/core/billing"
)

// Fake records calls and serves subscriptions and checkout sessions that
// tests put in it.
type Fake struct {
	mu            sync.Mutex
	Subscriptions map[string]*stripe.Subscription
	Sessions      map[string]*stripe.CheckoutSession
	Customers     map[string]string // orgID -> customer ID
	Checkouts     []billing.CheckoutParams
	Canceled      []string
	Fetches       int
	ItemUpdates   int
	next          int
}

func New() *Fake {
	return &Fake{
		Subscriptions: map[string]*stripe.Subscription{},
		Sessions:      map[string]*stripe.CheckoutSession{},
		Customers:     map[string]string{},
	}
}

// Subscription builds a subscription on plan with the given status and
// stores it.
func (f *Fake) Subscription(id, customerID string, plan billing.Plan, status stripe.SubscriptionStatus) *stripe.Subscription {
	sub := &stripe.Subscription{
		ID: id, Status: status, Customer: &stripe.Customer{ID: customerID},
		Items: &stripe.SubscriptionItemList{Data: []*stripe.SubscriptionItem{{
			ID: "si_" + id, Price: &stripe.Price{ID: "price_" + plan.Tier, LookupKey: plan.LookupKey},
			CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour).Unix(),
		}}},
	}
	f.mu.Lock()
	f.Subscriptions[id] = sub
	f.mu.Unlock()
	return sub
}

func (f *Fake) CreateCustomer(_ context.Context, orgID, _, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.Customers[orgID]; ok {
		return id, nil
	}
	f.next++
	id := fmt.Sprintf("cus_fake_%s_%d", orgID[:8], f.next)
	f.Customers[orgID] = id
	return id, nil
}

func (f *Fake) CreateCheckoutSession(_ context.Context, p billing.CheckoutParams) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Checkouts = append(f.Checkouts, p)
	return "https://checkout.stripe.test/c/" + p.Plan.Tier, nil
}

func (f *Fake) GetCheckoutSession(_ context.Context, id string) (*stripe.CheckoutSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.Sessions[id]; ok {
		return s, nil
	}
	return nil, errors.New("fake: no such checkout session")
}

func (f *Fake) GetSubscription(_ context.Context, id string) (*stripe.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Fetches++
	if s, ok := f.Subscriptions[id]; ok {
		return s, nil
	}
	return nil, errors.New("fake: no such subscription")
}

func (f *Fake) ChangePlan(_ context.Context, sub *stripe.Subscription, plan billing.Plan) (*stripe.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.Subscriptions[sub.ID]
	if s == nil {
		return nil, errors.New("fake: no such subscription")
	}
	s.Items.Data[0].Price = &stripe.Price{ID: "price_" + plan.Tier, LookupKey: plan.LookupKey}
	return s, nil
}

func (f *Fake) CancelSubscription(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Canceled = append(f.Canceled, id)
	if s, ok := f.Subscriptions[id]; ok {
		s.Status = stripe.SubscriptionStatusCanceled
	}
	return nil
}

func (f *Fake) CreatePortalSession(_ context.Context, customerID, _ string) (string, error) {
	return "https://billing.stripe.test/p/" + customerID, nil
}

// UpdateItems applies changes to the stored subscription's items.
func (f *Fake) UpdateItems(_ context.Context, subID string, changes []billing.ItemChange) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.Subscriptions[subID]
	if s == nil {
		return errors.New("fake: no such subscription")
	}
	f.ItemUpdates++
	for _, ch := range changes {
		switch {
		case ch.Delete:
			kept := s.Items.Data[:0]
			for _, it := range s.Items.Data {
				if it.ID != ch.ItemID {
					kept = append(kept, it)
				}
			}
			s.Items.Data = kept
		case ch.ItemID != "":
			for _, it := range s.Items.Data {
				if it.ID == ch.ItemID {
					it.Quantity = ch.Quantity
				}
			}
		default:
			f.next++
			s.Items.Data = append(s.Items.Data, &stripe.SubscriptionItem{
				ID: fmt.Sprintf("si_extra_%d", f.next), Quantity: ch.Quantity,
				Price: &stripe.Price{ID: "price_" + ch.LookupKey, LookupKey: ch.LookupKey},
			})
		}
	}
	return nil
}

// ExtraItems returns the subscription's extra-workspace items as
// lookup key -> quantity.
func (f *Fake) ExtraItems(subID string) map[string]int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int64{}
	for _, it := range f.Subscriptions[subID].Items.Data {
		if billing.IsExtraWorkspaceLookupKey(it.Price.LookupKey) {
			out[it.Price.LookupKey] += it.Quantity
		}
	}
	return out
}

var _ billing.Stripe = (*Fake)(nil)

// Emails is a notify.EmailSender that records what it sends.
type Emails struct {
	mu   sync.Mutex
	Sent []string // recipient addresses
}

func (e *Emails) Send(_ context.Context, to, _, _, _ string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Sent = append(e.Sent, to)
	return nil
}

func (e *Emails) Count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.Sent)
}

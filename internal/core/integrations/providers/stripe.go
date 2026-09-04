package providers

import (
	"context"
)

// Stripe is a KeyProvider, not an OAuthProvider — the founder pastes a
// secret key. ValidateKey hits GET /v1/balance, the cheapest authenticated
// call, purely to confirm the key is live — no stripe-go dependency for a
// single REST call (see "Dependency policy" in CLAUDE.md).
type Stripe struct{}

func NewStripe() *Stripe { return &Stripe{} }

func (s *Stripe) Name() string { return "stripe" }

func (s *Stripe) ValidateKey(ctx context.Context, key string) error {
	// Stripe uses HTTP Basic auth (secret key as username, empty password),
	// not Bearer — bearerRequest doesn't fit here.
	return basicAuthRequest(ctx, "GET", "https://api.stripe.com/v1/balance", key, "")
}

package providers

import (
	"context"
)

// Stripe is a KeyProvider, not an OAuthProvider — the founder pastes a
// secret key rather than authorizing an OAuth redirect. ValidateKey hits
// GET /v1/balance, the cheapest authenticated call in Stripe's API,
// purely to confirm the key is live — the response body itself is
// unused. Implemented as a plain REST call rather than pulling in
// stripe-go: one GET request doesn't justify a full SDK dependency for a
// bootstrapped, dependency-averse codebase (see the "Dependency policy"
// note in CLAUDE.md) — add stripe-go later if/when workflow 5's Stripe
// MCP tools need more than this.
type Stripe struct{}

func NewStripe() *Stripe { return &Stripe{} }

func (s *Stripe) Name() string { return "stripe" }

func (s *Stripe) ValidateKey(ctx context.Context, key string) error {
	// Stripe's API uses HTTP Basic auth with the secret key as the
	// username and an empty password — bearerRequest doesn't fit (Stripe
	// doesn't accept "Authorization: Bearer" on this endpoint), so this
	// builds the request directly.
	return basicAuthRequest(ctx, "GET", "https://api.stripe.com/v1/balance", key, "")
}

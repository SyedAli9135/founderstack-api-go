package integrations

import (
	"context"
	"time"
)

// Token is the normalized result of an OAuth exchange or refresh. Extra
// carries provider-specific fields (e.g. Discord's webhook URL) without
// forcing every other provider to know about them — a zero-cost extension
// point (nil map, omitted from JSON when empty).
type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time // zero value means "doesn't expire" (e.g. a Slack bot token)
	Scopes       []string
	Extra        map[string]string
}

// Provider is the minimum every catalog entry implements. Deliberately
// tiny — call sites type-assert up to whichever sub-interface below they
// actually need, instead of one fat interface every provider must satisfy.
type Provider interface {
	// Name is the catalog key (e.g. "slack"), not the display name.
	Name() string
}

// OAuthProvider: full OAuth 2.0 authorization-code flow (Slack, Discord,
// Notion, Google Drive, Google Calendar, LinkedIn).
type OAuthProvider interface {
	Provider
	// GetAuthURL returns the provider's authorization URL, embedding state
	// (state.GenerateState) for CSRF protection and callback recovery.
	GetAuthURL(state string) string
	ExchangeCode(ctx context.Context, code string) (*Token, error)
}

// Refreshable: OAuth providers whose tokens expire and renew without
// founder interaction (Google Drive/Calendar today). Others simply don't
// implement it — callers detect support via type assertion.
type Refreshable interface {
	RefreshAccessToken(ctx context.Context, refreshToken string) (*Token, error)
}

// Revocable: providers with a real revoke-token API. Not universal;
// DELETE .../{service} treats revocation as best-effort.
type Revocable interface {
	RevokeToken(ctx context.Context, token string) error
}

// TokenValidator: a cheap "is this token still good" call, used by
// GET .../{service}/status.
type TokenValidator interface {
	ValidateToken(ctx context.Context, token string) error
}

// KeyProvider: non-OAuth providers (Stripe API key, GitHub PAT) where
// "connecting" is pasting a credential, not an OAuth redirect.
type KeyProvider interface {
	Provider
	ValidateKey(ctx context.Context, key string) error
}

package integrations

import (
	"context"
	"time"
)

// Token is the normalized result of an OAuth exchange or refresh. Extra
// carries provider-specific fields that don't fit the common shape
// without forcing every other provider — or tokenstore.go — to know
// about them. Nothing in the catalog needs it today; kept because it's a
// zero-cost extension point (a nil map, omitted from the JSON envelope
// when empty) rather than a per-provider interface parameter every
// implementation would have to explicitly ignore.
type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time // zero value means "doesn't expire" (e.g. a Slack bot token)
	Scopes       []string
	Extra        map[string]string
}

// Provider is the minimum every catalog entry implements. Deliberately
// tiny — see the interface segregation note on the sub-interfaces below;
// a type asserts up to whichever capability a call site actually needs
// instead of a single interface every provider must satisfy in full.
type Provider interface {
	// Name is the catalog key (e.g. "slack"), not the display name.
	Name() string
}

// OAuthProvider is implemented by every service that requires a full
// OAuth 2.0 authorization-code flow: Slack, Discord, Notion, Google
// Drive, Google Calendar, LinkedIn.
type OAuthProvider interface {
	Provider
	// GetAuthURL returns the provider's authorization URL to redirect the
	// founder to, embedding state (from state.GenerateState) for CSRF
	// protection and later recovery of org_id/service in the callback.
	GetAuthURL(state string) string
	// ExchangeCode trades an authorization code (from the callback query
	// string) for a Token.
	ExchangeCode(ctx context.Context, code string) (*Token, error)
}

// Refreshable is implemented by OAuth providers whose access tokens
// expire and can be renewed without founder interaction (Google Drive,
// Google Calendar today). Slack/Discord/Notion/LinkedIn tokens don't
// expire the same way, so they simply don't implement this — callers
// detect support via a type assertion, not a capability flag.
type Refreshable interface {
	RefreshAccessToken(ctx context.Context, refreshToken string) (*Token, error)
}

// Revocable is implemented by providers with a real "revoke this token"
// API call. Not every OAuth provider exposes one; DELETE .../{service}
// treats revocation as best-effort via a type assertion, never a hard
// requirement.
type Revocable interface {
	RevokeToken(ctx context.Context, token string) error
}

// TokenValidator is implemented by providers that expose a cheap
// "is this token still good" call, used by GET .../{service}/status.
type TokenValidator interface {
	ValidateToken(ctx context.Context, token string) error
}

// KeyProvider is implemented by the non-OAuth providers — Stripe (API
// key), GitHub (PAT) — where "connecting" is the founder pasting a
// credential rather than an OAuth redirect.
type KeyProvider interface {
	Provider
	ValidateKey(ctx context.Context, key string) error
}

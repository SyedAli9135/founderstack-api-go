package providers

import "github.com/founderstack/api/internal/core/integrations"

// Compile-time documentation of exactly which capability each provider
// implements — a mismatch here (e.g. someone accidentally giving Notion
// a Refreshable it can't really support) fails the build, not silently
// at runtime via a failed type assertion deep in a handler.
var (
	_ integrations.OAuthProvider  = (*Slack)(nil)
	_ integrations.Revocable      = (*Slack)(nil)
	_ integrations.TokenValidator = (*Slack)(nil)

	_ integrations.OAuthProvider  = (*Discord)(nil)
	_ integrations.Revocable      = (*Discord)(nil)
	_ integrations.TokenValidator = (*Discord)(nil)

	_ integrations.OAuthProvider  = (*Notion)(nil)
	_ integrations.TokenValidator = (*Notion)(nil)

	_ integrations.OAuthProvider  = (*GoogleDrive)(nil)
	_ integrations.Refreshable    = (*GoogleDrive)(nil)
	_ integrations.Revocable      = (*GoogleDrive)(nil)
	_ integrations.TokenValidator = (*GoogleDrive)(nil)

	_ integrations.OAuthProvider  = (*GoogleCalendar)(nil)
	_ integrations.Refreshable    = (*GoogleCalendar)(nil)
	_ integrations.Revocable      = (*GoogleCalendar)(nil)
	_ integrations.TokenValidator = (*GoogleCalendar)(nil)

	_ integrations.OAuthProvider  = (*LinkedIn)(nil)
	_ integrations.Revocable      = (*LinkedIn)(nil)
	_ integrations.TokenValidator = (*LinkedIn)(nil)

	_ integrations.KeyProvider = (*Stripe)(nil)
	_ integrations.KeyProvider = (*GitHub)(nil)
)

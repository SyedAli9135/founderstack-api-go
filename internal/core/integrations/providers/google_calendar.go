package providers

import (
	"context"
	"net/url"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/founderstack/api/internal/core/integrations"
)

const calendarEventsScope = "https://www.googleapis.com/auth/calendar.events"

// GoogleCalendar shares its OAuth app (client ID/secret) with GoogleDrive —
// one Google Cloud project, two scopes — but is a distinct catalog entry /
// mcp_connections row, since a founder may connect one without the other.
type GoogleCalendar struct {
	cfg *oauth2.Config
}

func NewGoogleCalendar(clientID, clientSecret, redirectURL string) *GoogleCalendar {
	return &GoogleCalendar{cfg: &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     google.Endpoint,
		Scopes:       []string{calendarEventsScope},
	}}
}

func (g *GoogleCalendar) Name() string { return "google_calendar" }

func (g *GoogleCalendar) GetAuthURL(state string) string {
	return g.cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
}

func (g *GoogleCalendar) ExchangeCode(ctx context.Context, code string) (*integrations.Token, error) {
	t, err := g.cfg.Exchange(ctx, code)
	if err != nil {
		return nil, err
	}
	return toToken(t), nil
}

func (g *GoogleCalendar) RefreshAccessToken(ctx context.Context, refreshToken string) (*integrations.Token, error) {
	return refreshViaTokenSource(ctx, g.cfg, refreshToken)
}

func (g *GoogleCalendar) RevokeToken(ctx context.Context, token string) error {
	return simpleRequest(ctx, "POST", "https://oauth2.googleapis.com/revoke?token="+url.QueryEscape(token))
}

func (g *GoogleCalendar) ValidateToken(ctx context.Context, token string) error {
	return simpleRequest(ctx, "GET", "https://www.googleapis.com/oauth2/v3/tokeninfo?access_token="+url.QueryEscape(token))
}

package providers

import (
	"context"
	"net/url"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/founderstack/api/internal/core/integrations"
)

// driveFileScope is deliberately drive.file, not drive/drive.readonly —
// per-file access (only files this app creates or the founder explicitly
// opens with it) keeps this out of Google's "restricted scope" bucket and
// avoids the paid CASA security assessment full Drive access requires.
// See WORKFLOW_PLAN_GO.md's workflow 4 implementation note.
const driveFileScope = "https://www.googleapis.com/auth/drive.file"

type GoogleDrive struct {
	cfg *oauth2.Config
}

func NewGoogleDrive(clientID, clientSecret, redirectURL string) *GoogleDrive {
	return &GoogleDrive{cfg: &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     google.Endpoint,
		Scopes:       []string{driveFileScope},
	}}
}

func (g *GoogleDrive) Name() string { return "google_drive" }

func (g *GoogleDrive) GetAuthURL(state string) string {
	// AccessTypeOffline + ApprovalForce: without both, Google only issues a
	// refresh_token on a user's *first* authorization ever for this
	// app+scope combination — a founder reconnecting after a revoke would
	// silently get an access-only token with nothing for the refresh job
	// to renew it with.
	return g.cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
}

func (g *GoogleDrive) ExchangeCode(ctx context.Context, code string) (*integrations.Token, error) {
	t, err := g.cfg.Exchange(ctx, code)
	if err != nil {
		return nil, err
	}
	return toToken(t), nil
}

func (g *GoogleDrive) RefreshAccessToken(ctx context.Context, refreshToken string) (*integrations.Token, error) {
	return refreshViaTokenSource(ctx, g.cfg, refreshToken)
}

func (g *GoogleDrive) RevokeToken(ctx context.Context, token string) error {
	return simpleRequest(ctx, "POST", "https://oauth2.googleapis.com/revoke?token="+url.QueryEscape(token))
}

func (g *GoogleDrive) ValidateToken(ctx context.Context, token string) error {
	return simpleRequest(ctx, "GET", "https://www.googleapis.com/oauth2/v3/tokeninfo?access_token="+url.QueryEscape(token))
}

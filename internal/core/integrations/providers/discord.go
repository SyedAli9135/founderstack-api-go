package providers

import (
	"context"

	"golang.org/x/oauth2"

	"github.com/founderstack/api/internal/core/integrations"
)

// discordEndpoint is a standard RFC 6749 authorization-code flow — no
// provider-specific quirks (unlike Slack's always-200-with-an-"ok"-field
// response), so golang.org/x/oauth2 handles the exchange directly.
var discordEndpoint = oauth2.Endpoint{
	AuthURL:  "https://discord.com/api/oauth2/authorize",
	TokenURL: "https://discord.com/api/oauth2/token",
}

// discordScopes covers what the workflow 5 Slack-equivalent "post a
// notification" tool needs for Discord: read the founder's identity for
// the connection card, and obtain a webhook the agent can post through.
var discordScopes = []string{"identify", "webhook.incoming"}

type Discord struct {
	cfg *oauth2.Config
}

func NewDiscord(clientID, clientSecret, redirectURL string) *Discord {
	return &Discord{cfg: &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     discordEndpoint,
		Scopes:       discordScopes,
	}}
}

func (d *Discord) Name() string { return "discord" }

func (d *Discord) GetAuthURL(state string) string {
	return d.cfg.AuthCodeURL(state)
}

func (d *Discord) ExchangeCode(ctx context.Context, code string) (*integrations.Token, error) {
	t, err := d.cfg.Exchange(ctx, code)
	if err != nil {
		return nil, err
	}
	return toToken(t), nil
}

func (d *Discord) RevokeToken(ctx context.Context, token string) error {
	return revokeViaRFC7009(ctx, "https://discord.com/api/oauth2/token/revoke", d.cfg.ClientID, d.cfg.ClientSecret, token)
}

func (d *Discord) ValidateToken(ctx context.Context, token string) error {
	return bearerRequest(ctx, "GET", "https://discord.com/api/users/@me", token)
}

package providers

import (
	"context"

	"golang.org/x/oauth2"

	"github.com/founderstack/api/internal/core/integrations"
)

// discordEndpoint is a standard RFC 6749 authorization-code flow — no
// quirks like Slack's, so x/oauth2 handles the exchange directly.
var discordEndpoint = oauth2.Endpoint{
	AuthURL:  "https://discord.com/api/oauth2/authorize",
	TokenURL: "https://discord.com/api/oauth2/token",
}

// discordScopes: identify for the connection card, webhook.incoming to
// obtain a webhook the agent can post through.
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

// ExchangeCode also extracts the incoming webhook Discord's token response
// carries via the webhook.incoming scope — toToken alone only copies the
// standard OAuth2 fields, so this would otherwise be silently discarded.
// The webhook URL is the only way send_message can post anything; Discord
// has no general "post as this user" call reachable from these scopes.
func (d *Discord) ExchangeCode(ctx context.Context, code string) (*integrations.Token, error) {
	t, err := d.cfg.Exchange(ctx, code)
	if err != nil {
		return nil, err
	}
	tok := toToken(t)
	if webhook, ok := t.Extra("webhook").(map[string]interface{}); ok {
		tok.Extra = map[string]string{}
		if url, ok := webhook["url"].(string); ok {
			tok.Extra["webhook_url"] = url
		}
		if id, ok := webhook["id"].(string); ok {
			tok.Extra["webhook_id"] = id
		}
		if wtoken, ok := webhook["token"].(string); ok {
			tok.Extra["webhook_token"] = wtoken
		}
		if channelID, ok := webhook["channel_id"].(string); ok {
			tok.Extra["webhook_channel_id"] = channelID
		}
	}
	return tok, nil
}

func (d *Discord) RevokeToken(ctx context.Context, token string) error {
	return revokeViaRFC7009(ctx, "https://discord.com/api/oauth2/token/revoke", d.cfg.ClientID, d.cfg.ClientSecret, token)
}

func (d *Discord) ValidateToken(ctx context.Context, token string) error {
	return bearerRequest(ctx, "GET", "https://discord.com/api/users/@me", token)
}

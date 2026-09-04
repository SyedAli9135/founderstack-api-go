package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/oauth2"

	"github.com/founderstack/api/internal/core/integrations"
)

var linkedinEndpoint = oauth2.Endpoint{
	AuthURL:  "https://www.linkedin.com/oauth/v2/authorization",
	TokenURL: "https://www.linkedin.com/oauth/v2/accessToken",
}

// linkedinScopes is deliberately just w_member_social (the free "Share on
// LinkedIn" posting scope) — not openid/profile, which nothing here needs
// and would only add to what a founder has to trust when authorizing.
var linkedinScopes = []string{"w_member_social"}

type LinkedIn struct {
	cfg *oauth2.Config
}

func NewLinkedIn(clientID, clientSecret, redirectURL string) *LinkedIn {
	return &LinkedIn{cfg: &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     linkedinEndpoint,
		Scopes:       linkedinScopes,
	}}
}

func (l *LinkedIn) Name() string { return "linkedin" }

func (l *LinkedIn) GetAuthURL(state string) string {
	return l.cfg.AuthCodeURL(state)
}

func (l *LinkedIn) ExchangeCode(ctx context.Context, code string) (*integrations.Token, error) {
	t, err := l.cfg.Exchange(ctx, code)
	if err != nil {
		return nil, err
	}
	return toToken(t), nil
}

func (l *LinkedIn) RevokeToken(ctx context.Context, token string) error {
	return revokeViaRFC7009(ctx, "https://www.linkedin.com/oauth/v2/revoke", l.cfg.ClientID, l.cfg.ClientSecret, token)
}

// ValidateToken uses token introspection, not a resource API — with only
// w_member_social granted there's no profile endpoint to call, but
// introspection is client-authenticated rather than scope-gated.
func (l *LinkedIn) ValidateToken(ctx context.Context, token string) error {
	form := url.Values{
		"token":         {token},
		"client_id":     {l.cfg.ClientID},
		"client_secret": {l.cfg.ClientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://www.linkedin.com/oauth/v2/introspectToken", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("providers: build linkedin introspect request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("providers: linkedin introspect request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("providers: linkedin introspect returned %d", resp.StatusCode)
	}

	var result struct {
		Active bool `json:"active"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("providers: decode linkedin introspect response: %w", err)
	}
	if !result.Active {
		return fmt.Errorf("providers: linkedin token is not active")
	}
	return nil
}

package providers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/oauth2"

	"github.com/founderstack/api/internal/core/integrations"
)

// toToken converts an *oauth2.Token into our normalized integrations.Token.
// A zero Expiry ("provider didn't say") is treated as "never expires" by
// tokenstore.go — there's nothing to guess at when expires_in is omitted.
func toToken(t *oauth2.Token) *integrations.Token {
	return &integrations.Token{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		ExpiresAt:    t.Expiry,
	}
}

// refreshViaTokenSource is the shared Refreshable implementation for every
// standard-OAuth2 provider — x/oauth2's TokenSource already knows how to
// exchange a refresh token, so no provider hand-rolls its own refresh call.
func refreshViaTokenSource(ctx context.Context, cfg *oauth2.Config, refreshToken string) (*integrations.Token, error) {
	src := cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken})
	t, err := src.Token()
	if err != nil {
		return nil, fmt.Errorf("providers: refresh access token: %w", err)
	}
	return toToken(t), nil
}

// revokeViaRFC7009 posts to revokeURL per RFC 7009 (Discord/Google/
// LinkedIn's shape). Providers with a different revoke shape (Basic auth +
// JSON) implement their own rather than force-fitting this helper.
func revokeViaRFC7009(ctx context.Context, revokeURL, clientID, clientSecret, token string) error {
	form := url.Values{
		"token":         {token},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, revokeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("providers: build revoke request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("providers: revoke request to %s: %w", revokeURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("providers: revoke at %s returned %d: %s", revokeURL, resp.StatusCode, body)
	}
	return nil
}

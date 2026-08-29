package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/founderstack/api/internal/core/integrations"
)

// slackScopes is comma-separated (Slack's own convention for its "scope"
// query/form parameter — unlike standard OAuth2's space-separated list),
// which is one of the reasons Slack is implemented by hand below rather
// than through golang.org/x/oauth2.Config: the other is that every Slack
// API response, including a *failed* token exchange, comes back as HTTP
// 200 with an "ok": false body — x/oauth2's Exchange treats any 200 as
// success and would silently return a token-shaped struct with an empty
// access_token instead of an error.
//
// groups:read is required alongside channels:read — real bug caught by
// live manual verification 2026-08-28: internal/core/mcp/servers/slack.go's
// list_channels calls conversations.list with
// types=public_channel,private_channel, and Slack requires the scope for
// *every* requested type to be present or it rejects the whole call with
// missing_scope, even though channels:read alone is enough for the
// public_channel half. Without groups:read this tool could never
// succeed for any org.
const slackScopes = "chat:write,channels:read,groups:read"

type Slack struct {
	clientID     string
	clientSecret string
	redirectURL  string
}

func NewSlack(clientID, clientSecret, redirectURL string) *Slack {
	return &Slack{clientID: clientID, clientSecret: clientSecret, redirectURL: redirectURL}
}

func (s *Slack) Name() string { return "slack" }

func (s *Slack) GetAuthURL(state string) string {
	q := url.Values{
		"client_id":    {s.clientID},
		"scope":        {slackScopes},
		"redirect_uri": {s.redirectURL},
		"state":        {state},
	}
	return "https://slack.com/oauth/v2/authorize?" + q.Encode()
}

// slackAPIResponse is the common envelope every Slack Web API call
// returns — ok is checked before anything else is trusted, regardless of
// HTTP status (which is always 200 on a well-formed request).
type slackAPIResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func (s *Slack) ExchangeCode(ctx context.Context, code string) (*integrations.Token, error) {
	form := url.Values{
		"client_id":     {s.clientID},
		"client_secret": {s.clientSecret},
		"code":          {code},
		"redirect_uri":  {s.redirectURL},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://slack.com/api/oauth.v2.access", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("providers: build slack exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("providers: slack exchange request: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		slackAPIResponse
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("providers: decode slack exchange response: %w", err)
	}
	if !body.OK {
		return nil, fmt.Errorf("providers: slack oauth.v2.access failed: %s", body.Error)
	}

	// Bot tokens (xoxb-...) issued this way don't expire and have no
	// refresh token under standard (non-rotating) OAuth — hence no
	// ExpiresAt/RefreshToken here, and Slack correctly doesn't implement
	// integrations.Refreshable.
	return &integrations.Token{AccessToken: body.AccessToken}, nil
}

func (s *Slack) RevokeToken(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/auth.revoke", nil)
	if err != nil {
		return fmt.Errorf("providers: build slack revoke request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("providers: slack revoke request: %w", err)
	}
	defer resp.Body.Close()

	var body slackAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("providers: decode slack revoke response: %w", err)
	}
	if !body.OK {
		return fmt.Errorf("providers: slack auth.revoke failed: %s", body.Error)
	}
	return nil
}

func (s *Slack) ValidateToken(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/auth.test", nil)
	if err != nil {
		return fmt.Errorf("providers: build slack validate request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("providers: slack validate request: %w", err)
	}
	defer resp.Body.Close()

	var body slackAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("providers: decode slack validate response: %w", err)
	}
	if !body.OK {
		return fmt.Errorf("providers: slack auth.test failed: %s", body.Error)
	}
	return nil
}

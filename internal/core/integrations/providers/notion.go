package providers

import (
	"context"
	"fmt"
	"net/http"

	"golang.org/x/oauth2"

	"github.com/founderstack/api/internal/core/integrations"
)

var notionEndpoint = oauth2.Endpoint{
	AuthURL:  "https://api.notion.com/v1/oauth/authorize",
	TokenURL: "https://api.notion.com/v1/oauth/token",
	// Notion requires HTTP Basic auth (client_id:client_secret) on the
	// token request, not form-encoded client credentials.
	AuthStyle: oauth2.AuthStyleInHeader,
}

// notionAPIVersion is required on every Notion API call (not just OAuth) —
// Notion has no implicit "latest" version; omitting the header 400s.
const notionAPIVersion = "2022-06-28"

// Notion has no public Refreshable/Revocable API — revoking access happens
// from the founder's own workspace settings, not through this backend. It
// implements only OAuthProvider and TokenValidator; DELETE .../notion
// still works (RevokeToken is skipped via the handler's type assertion).
type Notion struct {
	cfg *oauth2.Config
}

func NewNotion(clientID, clientSecret, redirectURL string) *Notion {
	return &Notion{cfg: &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     notionEndpoint,
	}}
}

func (n *Notion) Name() string { return "notion" }

func (n *Notion) GetAuthURL(state string) string {
	// owner=user is required by Notion's authorize endpoint — it has no
	// other valid value for a third-party integration like this one.
	return n.cfg.AuthCodeURL(state, oauth2.SetAuthURLParam("owner", "user"))
}

func (n *Notion) ExchangeCode(ctx context.Context, code string) (*integrations.Token, error) {
	t, err := n.cfg.Exchange(ctx, code)
	if err != nil {
		return nil, err
	}
	return toToken(t), nil
}

func (n *Notion) ValidateToken(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.notion.com/v1/users/me", nil)
	if err != nil {
		return fmt.Errorf("providers: build notion validate request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Notion-Version", notionAPIVersion)
	return doAndCheck(req)
}

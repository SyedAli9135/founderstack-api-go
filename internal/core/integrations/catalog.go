package integrations

// AuthType is UI/routing metadata only — dispatch actual provider
// behavior via Registry + interface assertions (types.go), not by
// switching on this string.
type AuthType string

const (
	AuthTypeOAuth  AuthType = "oauth"
	AuthTypeAPIKey AuthType = "api_key"
	AuthTypePAT    AuthType = "pat"
)

// Meta describes one catalog entry for GET /api/v1/integrations and the
// frontend's integration grid.
type Meta struct {
	Name     string
	AuthType AuthType
	Category string
}

// Catalog is the static list of every integration this backend supports.
var Catalog = map[string]Meta{
	"slack":           {Name: "Slack", AuthType: AuthTypeOAuth, Category: "Communication"},
	"discord":         {Name: "Discord", AuthType: AuthTypeOAuth, Category: "Communication"},
	"notion":          {Name: "Notion", AuthType: AuthTypeOAuth, Category: "Productivity"},
	"google_drive":    {Name: "Google Drive", AuthType: AuthTypeOAuth, Category: "Productivity"},
	"google_calendar": {Name: "Google Calendar", AuthType: AuthTypeOAuth, Category: "Productivity"},
	"stripe":          {Name: "Stripe", AuthType: AuthTypeAPIKey, Category: "Finance"},
	"github":          {Name: "GitHub", AuthType: AuthTypePAT, Category: "Code"},
	"linkedin":        {Name: "LinkedIn", AuthType: AuthTypeOAuth, Category: "Marketing"},
}

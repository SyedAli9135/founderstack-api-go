package servers

import gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

// AllServers returns one fresh *mcp.Server per tool server in this
// package, keyed by the same service name internal/core/integrations
// uses. Both cmd/api/main.go and cmd/seedtools need this identical map —
// factored out here (rather than duplicated in both, or built inside
// internal/core/mcp, which would create an import cycle: mcp is imported
// BY servers, not the other way around) so a new tool server is one line
// added once, not two kept in sync by hand.
func AllServers() map[string]*gomcp.Server {
	return map[string]*gomcp.Server{
		"stripe":          NewStripeServer(),
		"slack":           NewSlackServer(),
		"github":          NewGitHubServer(),
		"notion":          NewNotionServer(),
		"linkedin":        NewLinkedInServer(),
		"discord":         NewDiscordServer(),
		"google_drive":    NewGoogleDriveServer(),
		"google_calendar": NewGoogleCalendarServer(),
	}
}

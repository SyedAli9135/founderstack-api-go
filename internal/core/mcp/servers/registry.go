package servers

import gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

// AllServers is factored out here, not in internal/core/mcp, to avoid an
// import cycle (mcp is imported BY servers) — the one map both
// cmd/api/main.go and cmd/seedtools build their registry from.
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

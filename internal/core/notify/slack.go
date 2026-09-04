package notify

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	coremcp "github.com/founderstack/api/internal/core/mcp"
)

// Reuses the slack.send_message MCP tool rather than a second Slack
// client. Logs and swallows every failure — a notification failing must
// never affect the run it's about.
func sendSlackApproval(ctx context.Context, gateway *coremcp.Gateway, orgID pgtype.UUID, channel, text string) {
	if gateway == nil || channel == "" {
		return
	}
	_, err := gateway.ExecuteTool(ctx, orgID, "slack", "send_message", map[string]any{
		"channel": channel, "text": text,
	}, "")
	if err != nil {
		slog.Warn("notify: slack approval notification failed", "org_id", orgID, "channel", channel, "err", err)
	}
}

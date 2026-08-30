package notify

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	coremcp "github.com/founderstack/api/internal/core/mcp"
)

// sendSlackApproval posts text to the org's configured approvals channel
// via the existing slack.send_message MCP tool — the same call path any
// agent-driven tool call goes through, reused here rather than a second
// Slack client. Logs and swallows every failure (channel unset, org never
// connected Slack, tool call error): a notification failing must never
// affect the run it's about, the same "an audit-trail write failing
// shouldn't take down the run" reasoning internal/core/graph/observability.go
// already documents.
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

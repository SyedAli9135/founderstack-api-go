package servers

import (
	"context"
	"fmt"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	mcp "github.com/founderstack/api/internal/core/mcp"
)

// NewDiscordServer builds the Discord MCP tool server — send_message
// only. workflow 4 scoped Discord's OAuth to `identify` + `webhook.incoming`
// (see internal/core/integrations/providers/discord.go), which grants
// exactly one capability: posting through a single incoming webhook bound
// to whichever channel the founder picked during the OAuth consent
// screen. There is no channel-listing or general "post as this user" API
// reachable from this grant — Discord's real messaging API needs a bot
// token with guild permissions, which this integration was deliberately
// not built to request. A list_channels tool would have nothing to call.
func NewDiscordServer() *gomcp.Server {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "discord", Version: "1.0.0"}, nil)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "send_message",
		Description: "Post a message to the Discord channel this integration's incoming webhook is bound to.",
	}, discordSendMessage)

	return server
}

type discordSendMessageInput struct {
	Content string `json:"content" jsonschema:"Message text to post"`
}

type discordSendMessageOutput struct {
	MessageID string `json:"message_id"`
}

type discordWebhookResponse struct {
	ID string `json:"id"`
}

func discordSendMessage(ctx context.Context, req *gomcp.CallToolRequest, in discordSendMessageInput) (*gomcp.CallToolResult, discordSendMessageOutput, error) {
	extra, ok := mcp.ExtraFromRequest(req)
	webhookURL := extra["webhook_url"]
	if !ok || webhookURL == "" {
		return nil, discordSendMessageOutput{}, fmt.Errorf("discord: no incoming webhook on file for this connection — reconnect Discord to grant one")
	}
	if in.Content == "" {
		return nil, discordSendMessageOutput{}, fmt.Errorf("discord: content is required")
	}

	// ?wait=true makes Discord return the created message (so we can
	// report its id) instead of a bare 204 No Content. The webhook URL
	// itself is the credential — Discord webhooks take no Authorization
	// header at all, the token is embedded in the URL path.
	httpReq, err := newRequestWithBody(ctx, "POST", webhookURL+"?wait=true", map[string]string{"content": in.Content})
	if err != nil {
		return nil, discordSendMessageOutput{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	var resp discordWebhookResponse
	if err := doAndDecode(httpReq, &resp); err != nil {
		return nil, discordSendMessageOutput{}, fmt.Errorf("discord: send message: %w", err)
	}

	return nil, discordSendMessageOutput{MessageID: resp.ID}, nil
}

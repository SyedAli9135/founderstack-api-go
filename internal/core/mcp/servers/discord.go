package servers

import (
	"context"
	"fmt"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	mcp "github.com/founderstack/api/internal/core/mcp"
)

// send_message only: this OAuth grant (identify + webhook.incoming) only
// gives posting through one incoming webhook bound to whatever channel
// the founder picked at consent — no channel-listing or general messaging
// API is reachable without a bot token, which was deliberately not
// requested.
func NewDiscordServer() *gomcp.Server {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "discord", Version: "1.0.0"}, nil)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "send_message",
		Description: "Post a message to the Discord channel this integration's incoming webhook is bound to.",
		Annotations: mcp.ReversibleWrite(),
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

	// ?wait=true returns the created message instead of a bare 204. The
	// webhook URL itself is the credential — no Authorization header.
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

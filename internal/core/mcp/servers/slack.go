package servers

import (
	"context"
	"fmt"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	mcp "github.com/founderstack/api/internal/core/mcp"
)

// slackAPIBase is a var so slack_test.go can point it at a fake server —
// see stripe.go's stripeAPIBase for the same reasoning.
var slackAPIBase = "https://slack.com/api"

// NewSlackServer builds the Slack MCP tool server (WORKFLOW_PLAN_GO.md
// workflow 5) — send_message (the agent posts daily briefs here) and
// list_channels. Slack's Web API returns HTTP 200 even on a failed call,
// with {"ok": false, "error": "..."} in the body — doJSON's status-code
// check alone would miss that, so every response here is decoded into a
// struct embedding slackEnvelope and checked via checkSlackOK. Same
// quirk internal/core/integrations/providers/slack.go's OAuth exchange
// already had to handle.
func NewSlackServer() *gomcp.Server {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "slack", Version: "1.0.0"}, nil)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "send_message",
		Description: "Send a message to a Slack channel.",
	}, slackSendMessage)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "list_channels",
		Description: "List channels the bot can see in the connected Slack workspace.",
	}, slackListChannels)

	return server
}

// slackEnvelope is embedded in every Slack API response struct — every
// Slack Web API call, success or failure, carries this shape.
type slackEnvelope struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func checkSlackOK(e slackEnvelope, action string) error {
	if !e.OK {
		return fmt.Errorf("slack: %s failed: %s", action, e.Error)
	}
	return nil
}

type slackSendMessageInput struct {
	Channel string `json:"channel" jsonschema:"Slack channel ID or name (e.g. C0123456789 or #general)"`
	Text    string `json:"text" jsonschema:"Message text to post"`
}

type slackSendMessageOutput struct {
	Channel   string `json:"channel"`
	Timestamp string `json:"timestamp"`
}

type slackPostMessageResponse struct {
	slackEnvelope
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

func slackSendMessage(ctx context.Context, req *gomcp.CallToolRequest, in slackSendMessageInput) (*gomcp.CallToolResult, slackSendMessageOutput, error) {
	token, ok := mcp.TokenFromRequest(req)
	if !ok {
		return nil, slackSendMessageOutput{}, fmt.Errorf("slack: no token in request metadata")
	}
	if in.Channel == "" || in.Text == "" {
		return nil, slackSendMessageOutput{}, fmt.Errorf("slack: channel and text are required")
	}

	body := map[string]string{"channel": in.Channel, "text": in.Text}
	var resp slackPostMessageResponse
	if err := doJSON(ctx, "POST", slackAPIBase+"/chat.postMessage", token, body, &resp); err != nil {
		return nil, slackSendMessageOutput{}, fmt.Errorf("slack: send message: %w", err)
	}
	if err := checkSlackOK(resp.slackEnvelope, "chat.postMessage"); err != nil {
		return nil, slackSendMessageOutput{}, err
	}

	return nil, slackSendMessageOutput{Channel: resp.Channel, Timestamp: resp.TS}, nil
}

type slackListChannelsInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"Maximum channels to return (default 100, max 200)"`
}

type slackChannelSummary struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsPrivate bool   `json:"is_private"`
}

type slackListChannelsOutput struct {
	Channels []slackChannelSummary `json:"channels"`
}

type slackConversationsListResponse struct {
	slackEnvelope
	Channels []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		IsPrivate bool   `json:"is_private"`
	} `json:"channels"`
}

func slackListChannels(ctx context.Context, req *gomcp.CallToolRequest, in slackListChannelsInput) (*gomcp.CallToolResult, slackListChannelsOutput, error) {
	token, ok := mcp.TokenFromRequest(req)
	if !ok {
		return nil, slackListChannelsOutput{}, fmt.Errorf("slack: no token in request metadata")
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}

	endpoint := fmt.Sprintf("%s/conversations.list?limit=%d&types=public_channel,private_channel", slackAPIBase, limit)
	var resp slackConversationsListResponse
	if err := doJSON(ctx, "GET", endpoint, token, nil, &resp); err != nil {
		return nil, slackListChannelsOutput{}, fmt.Errorf("slack: list channels: %w", err)
	}
	if err := checkSlackOK(resp.slackEnvelope, "conversations.list"); err != nil {
		return nil, slackListChannelsOutput{}, err
	}

	out := slackListChannelsOutput{Channels: make([]slackChannelSummary, 0, len(resp.Channels))}
	for _, ch := range resp.Channels {
		out.Channels = append(out.Channels, slackChannelSummary{ID: ch.ID, Name: ch.Name, IsPrivate: ch.IsPrivate})
	}
	return nil, out, nil
}

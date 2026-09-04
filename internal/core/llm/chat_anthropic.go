package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// anthropicMessagesURL is a var so tests can point it at a fake httptest
// server.
var anthropicMessagesURL = "https://api.anthropic.com/v1/messages"

const (
	anthropicAPIVersion = "2023-06-01"
	// anthropicMaxTokens is a fixed ceiling for now; will become a
	// per-call param once agents.max_output_tokens is wired through.
	anthropicMaxTokens = 4096
)

// AnthropicChatClient implements ChatClient via plain net/http against
// Anthropic's Messages API, deliberately not anthropic-sdk-go — the
// single-call transport only; graph.Engine owns the call/tool/repeat loop.
type AnthropicChatClient struct {
	apiKey string
	model  string
}

// NewAnthropicChatClient builds a client for one org's decrypted BYOK key
// and the agent's configured model.
func NewAnthropicChatClient(apiKey, model string) *AnthropicChatClient {
	return &AnthropicChatClient{apiKey: apiKey, model: model}
}

// --- wire types ---

type anthropicSystemBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicCacheControl struct {
	Type string `json:"type"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicRequest struct {
	Model     string                 `json:"model"`
	MaxTokens int                    `json:"max_tokens"`
	System    []anthropicSystemBlock `json:"system,omitempty"`
	Messages  []anthropicMessage     `json:"messages"`
	Tools     []anthropicTool        `json:"tools,omitempty"`
}

type anthropicUsage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

type anthropicResponse struct {
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
}

// Send implements ChatClient.
func (c *AnthropicChatClient) Send(ctx context.Context, systemPrompt string, messages []Message, tools []ToolSchema) (ChatResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req := anthropicRequest{
		Model:     c.model,
		MaxTokens: anthropicMaxTokens,
		Messages:  toAnthropicMessages(messages),
		Tools:     toAnthropicTools(tools),
	}
	if systemPrompt != "" {
		// Cached: static for the whole run, the textbook prompt-caching case.
		req.System = []anthropicSystemBlock{{
			Type:         "text",
			Text:         systemPrompt,
			CacheControl: &anthropicCacheControl{Type: "ephemeral"},
		}}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: marshal anthropic request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicMessagesURL, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("%w: %w", ErrChatUnavailable, err)
	}
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicAPIVersion)
	httpReq.Header.Set("content-type", "application/json")

	resp, err := chatHTTPClient.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("%w: %w", ErrChatUnavailable, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("%w: %w", ErrChatUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return ChatResponse{}, classifyChatError(resp.StatusCode, respBody)
	}

	var out anthropicResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return ChatResponse{}, fmt.Errorf("%w: unmarshal response: %w", ErrChatUnavailable, err)
	}

	return fromAnthropicResponse(out), nil
}

func toAnthropicMessages(messages []Message) []anthropicMessage {
	out := make([]anthropicMessage, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case RoleUser:
			out = append(out, anthropicMessage{
				Role:    "user",
				Content: []anthropicContentBlock{{Type: "text", Text: m.Content}},
			})
		case RoleAssistant:
			var blocks []anthropicContentBlock
			if m.Content != "" {
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropicContentBlock{
					Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: tc.Args,
				})
			}
			out = append(out, anthropicMessage{Role: "assistant", Content: blocks})
		case RoleTool:
			// Anthropic has only user/assistant roles — a tool result is
			// a tool_result content block inside a user-role turn.
			out = append(out, anthropicMessage{
				Role: "user",
				Content: []anthropicContentBlock{{
					Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content, IsError: m.IsError,
				}},
			})
		}
	}
	return out
}

func toAnthropicTools(tools []ToolSchema) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropicTool, len(tools))
	for i, t := range tools {
		out[i] = anthropicTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
	}
	return out
}

func fromAnthropicResponse(r anthropicResponse) ChatResponse {
	out := ChatResponse{
		Usage: TokenUsage{
			InputTokens:  r.Usage.InputTokens,
			OutputTokens: r.Usage.OutputTokens,
			CachedTokens: r.Usage.CacheReadInputTokens,
		},
	}
	for _, block := range r.Content {
		switch block.Type {
		case "text":
			out.Content += block.Text
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: block.ID, Name: block.Name, Args: block.Input})
		}
	}
	switch r.StopReason {
	case "tool_use":
		out.StopReason = StopReasonToolUse
	case "end_turn", "stop_sequence":
		out.StopReason = StopReasonEndTurn
	case "max_tokens":
		out.StopReason = StopReasonMaxTokens
	default:
		out.StopReason = StopReasonOther
	}
	return out
}

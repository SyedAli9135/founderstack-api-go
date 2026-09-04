package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const openAIChatCompletionsPath = "/chat/completions"

const (
	openAIBaseURL   = "https://api.openai.com/v1"
	qwenBaseURL     = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	deepSeekBaseURL = "https://api.deepseek.com/v1"
)

// OpenAICompatibleChatClient implements ChatClient via plain net/http
// against any provider exposing an OpenAI-compatible
// POST {baseURL}/chat/completions endpoint — OpenAI, Qwen, and DeepSeek
// all mirror OpenAI's API shape, so one implementation covers all 3.
type OpenAICompatibleChatClient struct {
	baseURL string
	apiKey  string
	model   string
}

// NewOpenAICompatibleChatClient builds a client against baseURL (one of
// openAIBaseURL/qwenBaseURL/deepSeekBaseURL) for one org's decrypted BYOK
// key and the agent's configured model.
func NewOpenAICompatibleChatClient(baseURL, apiKey, model string) *OpenAICompatibleChatClient {
	return &OpenAICompatibleChatClient{baseURL: baseURL, apiKey: apiKey, model: model}
}

// --- wire types (OpenAI's Chat Completions API shape) ---

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIToolCallFunc `json:"function"`
}

type openAIToolCallFunc struct {
	Name string `json:"name"`
	// Arguments is a JSON-encoded string on the wire (not a raw object) —
	// this is OpenAI's actual convention, unlike Anthropic/Gemini's native
	// JSON input.
	Arguments string `json:"arguments"`
}

type openAITool struct {
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Tools    []openAITool    `json:"tools,omitempty"`
}

type openAIChoice struct {
	Message      openAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type openAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type openAIResponse struct {
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
}

// Send implements ChatClient.
func (c *OpenAICompatibleChatClient) Send(ctx context.Context, systemPrompt string, messages []Message, tools []ToolSchema) (ChatResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req := openAIRequest{
		Model:    c.model,
		Messages: toOpenAIMessages(systemPrompt, messages),
		Tools:    toOpenAITools(tools),
	}

	body, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: marshal openai-compatible request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+openAIChatCompletionsPath, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("%w: %w", ErrChatUnavailable, err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

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

	var out openAIResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return ChatResponse{}, fmt.Errorf("%w: unmarshal response: %w", ErrChatUnavailable, err)
	}
	if len(out.Choices) == 0 {
		return ChatResponse{}, fmt.Errorf("%w: response had no choices", ErrChatUnavailable)
	}

	return fromOpenAIResponse(out)
}

func toOpenAIMessages(systemPrompt string, messages []Message) []openAIMessage {
	out := make([]openAIMessage, 0, len(messages)+1)
	if systemPrompt != "" {
		out = append(out, openAIMessage{Role: "system", Content: systemPrompt})
	}
	for _, m := range messages {
		switch m.Role {
		case RoleUser:
			out = append(out, openAIMessage{Role: "user", Content: m.Content})
		case RoleAssistant:
			msg := openAIMessage{Role: "assistant", Content: m.Content}
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, openAIToolCall{
					ID: tc.ID, Type: "function",
					Function: openAIToolCallFunc{Name: tc.Name, Arguments: string(tc.Args)},
				})
			}
			out = append(out, msg)
		case RoleTool:
			out = append(out, openAIMessage{Role: "tool", Content: m.Content, ToolCallID: m.ToolCallID})
		}
	}
	return out
}

func toOpenAITools(tools []ToolSchema) []openAITool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openAITool, len(tools))
	for i, t := range tools {
		out[i] = openAITool{
			Type: "function",
			Function: openAIToolFunction{
				Name: t.Name, Description: t.Description, Parameters: t.InputSchema,
			},
		}
	}
	return out
}

func fromOpenAIResponse(r openAIResponse) (ChatResponse, error) {
	choice := r.Choices[0]
	out := ChatResponse{
		Content: choice.Message.Content,
		Usage: TokenUsage{
			InputTokens:  r.Usage.PromptTokens,
			OutputTokens: r.Usage.CompletionTokens,
			CachedTokens: r.Usage.PromptTokensDetails.CachedTokens,
		},
	}
	for _, tc := range choice.Message.ToolCalls {
		// Arguments arrives as a JSON-encoded string; re-parse into
		// json.RawMessage so ToolCall.Args is always a raw JSON value
		// regardless of which provider produced it.
		var args json.RawMessage
		if tc.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				return ChatResponse{}, fmt.Errorf("%w: tool call %q has invalid JSON arguments: %w", ErrChatUnavailable, tc.Function.Name, err)
			}
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: args})
	}

	switch choice.FinishReason {
	case "tool_calls":
		out.StopReason = StopReasonToolUse
	case "stop":
		out.StopReason = StopReasonEndTurn
	case "length":
		out.StopReason = StopReasonMaxTokens
	default:
		out.StopReason = StopReasonOther
	}
	return out, nil
}

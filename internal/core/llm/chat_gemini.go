package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// geminiGenerateContentBaseURL is a var so tests can point it at a fake
// httptest server — same pattern as verify.go's geminiModelsURL.
var geminiGenerateContentBaseURL = "https://generativelanguage.googleapis.com/v1beta/models"

// GeminiChatClient implements ChatClient via a plain net/http call
// against Google's generateContent API. Gets its own implementation
// (not folded into OpenAICompatibleChatClient) for the same reason
// verify.go's verifyGemini is separate from verifyOpenAICompatible: key
// auth is a `?key=` query param, not a Bearer header, and — new here —
// Gemini's tool-calling has no per-call ID concept at all, unlike
// Anthropic/OpenAI, which both assign one. A function result is matched
// back to its call by function *name*, not an ID, which is why
// Message.Name (not just ToolCallID) exists on RoleTool messages.
type GeminiChatClient struct {
	apiKey string
	model  string
}

// NewGeminiChatClient builds a client for one org's decrypted BYOK key
// and the agent's configured model.
func NewGeminiChatClient(apiKey, model string) *GeminiChatClient {
	return &GeminiChatClient{apiKey: apiKey, model: model}
}

// --- wire types (Gemini's generateContent shape) ---

type geminiPart struct {
	Text         string                `json:"text,omitempty"`
	FunctionCall *geminiFunctionCall   `json:"functionCall,omitempty"`
	FunctionResp *geminiFunctionResult `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiFunctionResult struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations"`
}

type geminiRequest struct {
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	Contents          []geminiContent `json:"contents"`
	Tools             []geminiTool    `json:"tools,omitempty"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
}

type geminiUsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate   `json:"candidates"`
	UsageMetadata geminiUsageMetadata `json:"usageMetadata"`
}

// Send implements ChatClient.
func (c *GeminiChatClient) Send(ctx context.Context, systemPrompt string, messages []Message, tools []ToolSchema) (ChatResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req := geminiRequest{
		Contents: toGeminiContents(messages),
		Tools:    toGeminiTools(tools),
	}
	if systemPrompt != "" {
		req.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: systemPrompt}}}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: marshal gemini request: %w", err)
	}

	url := geminiGenerateContentBaseURL + "/" + c.model + ":generateContent?key=" + c.apiKey
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("%w: %w", ErrChatUnavailable, err)
	}
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
		// Google returns 400, not 401/403, for a bad/invalid key — same
		// quirk verify.go's verifyGemini already documents. classifyChatError
		// already treats 400 as ErrChatRejected, so no special-casing needed.
		return ChatResponse{}, classifyChatError(resp.StatusCode, respBody)
	}

	var out geminiResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return ChatResponse{}, fmt.Errorf("%w: unmarshal response: %w", ErrChatUnavailable, err)
	}
	if len(out.Candidates) == 0 {
		return ChatResponse{}, fmt.Errorf("%w: response had no candidates", ErrChatUnavailable)
	}

	return fromGeminiResponse(out), nil
}

func toGeminiContents(messages []Message) []geminiContent {
	out := make([]geminiContent, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case RoleUser:
			out = append(out, geminiContent{Role: "user", Parts: []geminiPart{{Text: m.Content}}})
		case RoleAssistant:
			var parts []geminiPart
			if m.Content != "" {
				parts = append(parts, geminiPart{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{Name: tc.Name, Args: tc.Args}})
			}
			out = append(out, geminiContent{Role: "model", Parts: parts})
		case RoleTool:
			// Gemini has no per-call ID — a function result is matched
			// back to its call by name, carried on Message.Name (not
			// ToolCallID, which Gemini's wire format has no use for).
			response := map[string]any{"result": m.Content}
			if m.IsError {
				response = map[string]any{"error": m.Content}
			}
			out = append(out, geminiContent{
				Role:  "user",
				Parts: []geminiPart{{FunctionResp: &geminiFunctionResult{Name: m.Name, Response: response}}},
			})
		}
	}
	return out
}

func toGeminiTools(tools []ToolSchema) []geminiTool {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]geminiFunctionDeclaration, len(tools))
	for i, t := range tools {
		decls[i] = geminiFunctionDeclaration{Name: t.Name, Description: t.Description, Parameters: t.InputSchema}
	}
	return []geminiTool{{FunctionDeclarations: decls}}
}

func fromGeminiResponse(r geminiResponse) ChatResponse {
	candidate := r.Candidates[0]
	out := ChatResponse{
		Usage: TokenUsage{
			InputTokens:  r.UsageMetadata.PromptTokenCount,
			OutputTokens: r.UsageMetadata.CandidatesTokenCount,
			CachedTokens: r.UsageMetadata.CachedContentTokenCount,
		},
	}

	callIndex := 0
	for _, part := range candidate.Content.Parts {
		if part.Text != "" {
			out.Content += part.Text
		}
		if part.FunctionCall != nil {
			// Gemini assigns no call ID of its own — synthesize one
			// (stable within this response) so the rest of the engine's
			// generic ToolCall.ID-keyed machinery still works uniformly
			// across providers. A RoleTool reply for this call must carry
			// this same synthesized ID back on Message.ToolCallID (though
			// Gemini's own adapter ignores it and matches by Message.Name
			// instead — see toGeminiContents).
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:   "call_" + strconv.Itoa(callIndex),
				Name: part.FunctionCall.Name,
				Args: part.FunctionCall.Args,
			})
			callIndex++
		}
	}

	switch candidate.FinishReason {
	case "STOP":
		if len(out.ToolCalls) > 0 {
			out.StopReason = StopReasonToolUse
		} else {
			out.StopReason = StopReasonEndTurn
		}
	case "MAX_TOKENS":
		out.StopReason = StopReasonMaxTokens
	default:
		out.StopReason = StopReasonOther
	}
	return out
}

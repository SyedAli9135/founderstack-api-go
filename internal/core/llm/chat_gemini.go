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

// GeminiChatClient implements ChatClient via a plain net/http call against
// Google's generateContent API — its own implementation, not folded into
// OpenAICompatibleChatClient, because key auth is a `?key=` query param
// and Gemini's tool-calling has no per-call ID: a function result matches
// back to its call by name, hence Message.Name on RoleTool messages.
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
	// ThoughtSignature rides on the functionCall part it belongs to and must be
	// returned verbatim on later turns.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
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

	url := geminiGenerateContentBaseURL + "/" + c.model + ":generateContent"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("%w: %w", ErrChatUnavailable, err)
	}
	httpReq.Header.Set("x-goog-api-key", c.apiKey)
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
		// Google returns 400, not 401/403, for a bad key — classifyChatError
		// already treats 400 as ErrChatRejected.
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
				parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{Name: tc.Name, Args: tc.Args}, ThoughtSignature: tc.ThoughtSignature})
			}
			out = append(out, geminiContent{Role: "model", Parts: parts})
		case RoleTool:
			// Matched back to its call by Message.Name — Gemini's wire
			// format has no use for ToolCallID.
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
		decls[i] = geminiFunctionDeclaration{Name: t.Name, Description: t.Description, Parameters: sanitizeGeminiSchema(t.InputSchema)}
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
			// Gemini assigns no call ID — synthesize one so the engine's
			// generic ToolCall.ID-keyed machinery works uniformly across
			// providers; this adapter itself matches replies by name (see
			// toGeminiContents), not by this synthesized ID.
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:               "call_" + strconv.Itoa(callIndex),
				Name:             part.FunctionCall.Name,
				Args:             part.FunctionCall.Args,
				ThoughtSignature: part.ThoughtSignature,
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

// geminiSchemaKeys is the subset of JSON Schema Gemini's function_declarations
// accept (an OpenAPI-style schema). Anything else — notably the
// "additionalProperties" and "$schema" that MCP tool schemas carry — is
// rejected with a 400, so it is dropped rather than forwarded.
var geminiSchemaKeys = map[string]bool{
	"type": true, "format": true, "description": true, "nullable": true,
	"enum": true, "properties": true, "required": true, "items": true,
	"minItems": true, "maxItems": true, "minimum": true, "maximum": true,
}

// sanitizeGeminiSchema rewrites a JSON Schema into the subset Gemini accepts.
// It returns nil (no parameters) when the schema is empty or has no properties,
// since Gemini rejects an object schema with an empty properties map. On any
// parse problem it returns the input unchanged so the provider's own error
// surfaces instead of the tool silently disappearing.
func sanitizeGeminiSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	m, ok := cleanGeminiSchema(v).(map[string]any)
	if !ok {
		return raw
	}
	if props, _ := m["properties"].(map[string]any); len(props) == 0 {
		return nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

func cleanGeminiSchema(v any) any {
	switch t := v.(type) {
	case []any:
		for i := range t {
			t[i] = cleanGeminiSchema(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			switch k {
			case "properties":
				// Property names are arbitrary, so clean each value, not each key.
				props, ok := val.(map[string]any)
				if !ok {
					continue
				}
				cleaned := make(map[string]any, len(props))
				for name, p := range props {
					cleaned[name] = cleanGeminiSchema(p)
				}
				out[k] = cleaned
			case "type":
				// JSON Schema allows ["string","null"]; Gemini wants one type plus nullable.
				if arr, ok := val.([]any); ok {
					for _, e := range arr {
						if s, _ := e.(string); s == "null" {
							out["nullable"] = true
						} else if s != "" {
							out[k] = s
						}
					}
					continue
				}
				out[k] = val
			default:
				if geminiSchemaKeys[k] {
					out[k] = cleanGeminiSchema(val)
				}
			}
		}
		return out
	default:
		return v
	}
}

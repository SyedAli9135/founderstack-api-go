package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ModelInfo is one chat-capable model a provider's key can use.
type ModelInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// Model-list endpoints are vars so models_test.go can point them at httptest
// servers instead of the real provider APIs.
var (
	anthropicModelsURL = "https://api.anthropic.com/v1/models?limit=1000"
	openAIModelsURL    = "https://api.openai.com/v1/models"
	qwenModelsURL      = "https://dashscope.aliyuncs.com/compatible-mode/v1/models"
	deepSeekModelsURL  = "https://api.deepseek.com/v1/models"
)

// ListModels asks the provider which models apiKey can use and returns only
// chat-capable ones, sorted by ID. Model IDs are retired by providers without
// notice, so this live list — not a hardcoded table — is the source of truth
// for what an agent may run on.
//
// Errors wrap ErrKeyRejected (the provider refused the key) or
// ErrValidationUnavailable (couldn't reach it), same as key validation.
func ListModels(ctx context.Context, provider ProviderID, apiKey string) ([]ModelInfo, error) {
	var models []ModelInfo
	var err error
	switch provider {
	case ProviderAnthropic:
		models, err = listAnthropicModels(ctx, apiKey)
	case ProviderOpenAI:
		models, err = listOpenAICompatibleModels(ctx, openAIModelsURL, apiKey, isOpenAIChatModel)
	case ProviderQwen:
		models, err = listOpenAICompatibleModels(ctx, qwenModelsURL, apiKey, isQwenChatModel)
	case ProviderDeepSeek:
		models, err = listOpenAICompatibleModels(ctx, deepSeekModelsURL, apiKey, func(string) bool { return true })
	case ProviderGemini:
		models, err = listGeminiModels(ctx, apiKey)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, provider)
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

// PreferredModel picks the best default from a live list: the static default
// if the provider still offers it, otherwise the first model whose ID contains
// the provider's preferred family keyword, otherwise the first model. Returns
// "" for an empty list.
func PreferredModel(provider ProviderID, models []ModelInfo) string {
	if len(models) == 0 {
		return ""
	}
	want := DefaultModel(provider)
	for _, m := range models {
		if m.ID == want {
			return m.ID
		}
	}
	for _, kw := range preferredFamilies[provider] {
		for _, m := range models {
			if strings.Contains(m.ID, kw) && !strings.Contains(m.ID, "preview") {
				return m.ID
			}
		}
	}
	return models[0].ID
}

var preferredFamilies = map[ProviderID][]string{
	ProviderAnthropic: {"sonnet", "haiku"},
	ProviderOpenAI:    {"gpt-4o", "gpt-4", "gpt-"},
	ProviderGemini:    {"flash", "pro"},
	ProviderQwen:      {"qwen-plus", "qwen-turbo", "qwen"},
	ProviderDeepSeek:  {"chat"},
}

// HasModel reports whether id is in models.
func HasModel(models []ModelInfo, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}

func getJSON(ctx context.Context, url string, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrValidationUnavailable, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrValidationUnavailable, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest:
		return fmt.Errorf("%w: unexpected status %d", ErrKeyRejected, resp.StatusCode)
	default:
		return fmt.Errorf("%w: unexpected status %d", ErrValidationUnavailable, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%w: decode models: %w", ErrValidationUnavailable, err)
	}
	return nil
}

func listAnthropicModels(ctx context.Context, apiKey string) ([]ModelInfo, error) {
	var body struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := getJSON(ctx, anthropicModelsURL, map[string]string{"x-api-key": apiKey, "anthropic-version": "2023-06-01"}, &body); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(body.Data))
	for _, m := range body.Data {
		out = append(out, ModelInfo{ID: m.ID, DisplayName: firstNonEmpty(m.DisplayName, m.ID)})
	}
	return out, nil
}

func listOpenAICompatibleModels(ctx context.Context, url, apiKey string, keep func(id string) bool) ([]ModelInfo, error) {
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := getJSON(ctx, url, map[string]string{"Authorization": "Bearer " + apiKey}, &body); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(body.Data))
	for _, m := range body.Data {
		if keep(m.ID) {
			out = append(out, ModelInfo{ID: m.ID, DisplayName: m.ID})
		}
	}
	return out, nil
}

// OpenAI's list has no capability field, so non-chat families are excluded by
// name. A model this misjudges is still reachable through the agent form's
// free-text field.
func isOpenAIChatModel(id string) bool {
	chat := strings.HasPrefix(id, "gpt-") || strings.HasPrefix(id, "chatgpt-") ||
		(len(id) > 1 && id[0] == 'o' && id[1] >= '0' && id[1] <= '9')
	if !chat {
		return false
	}
	for _, bad := range []string{"embed", "tts", "whisper", "transcribe", "audio", "realtime", "image", "moderation", "search", "instruct"} {
		if strings.Contains(id, bad) {
			return false
		}
	}
	return true
}

func isQwenChatModel(id string) bool {
	if !(strings.HasPrefix(id, "qwen") || strings.HasPrefix(id, "qwq")) {
		return false
	}
	for _, bad := range []string{"embedding", "rerank", "tts", "asr", "image", "ocr", "mt-", "vl"} {
		if strings.Contains(id, bad) {
			return false
		}
	}
	return true
}

func listGeminiModels(ctx context.Context, apiKey string) ([]ModelInfo, error) {
	var body struct {
		Models []struct {
			Name        string   `json:"name"`
			DisplayName string   `json:"displayName"`
			Methods     []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := getJSON(ctx, geminiModelsURL+"?pageSize=200", map[string]string{"x-goog-api-key": apiKey}, &body); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(body.Models))
	for _, m := range body.Models {
		id := strings.TrimPrefix(m.Name, "models/")
		if !contains(m.Methods, "generateContent") || !isGeminiAgentModel(id) {
			continue
		}
		out = append(out, ModelInfo{ID: id, DisplayName: firstNonEmpty(m.DisplayName, id)})
	}
	return out, nil
}

// Gemma and the TTS/image/live variants answer generateContent but can't drive
// the function-calling loop an agent needs.
func isGeminiAgentModel(id string) bool {
	if !strings.HasPrefix(id, "gemini") {
		return false
	}
	for _, bad := range []string{"tts", "image", "live", "embedding", "audio", "robotics", "computer-use", "transcribe", "banana"} {
		if strings.Contains(id, bad) {
			return false
		}
	}
	return true
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ProbeTimeout bounds ProbeModel. Long enough for a slow first token, short
// enough that a stalled model is reported instead of hanging a settings page.
const ProbeTimeout = 25 * time.Second

// ProbeModel sends one tiny real request to confirm model actually answers.
// A provider's list can include models that then refuse every call (retired,
// "not available to new users") or stall, so listing alone can't prove a model
// is usable. The returned error is the provider's own wording, trimmed.
func ProbeModel(ctx context.Context, provider ProviderID, apiKey, model string) (time.Duration, error) {
	client, err := NewChatClient(provider, apiKey, model)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	start := time.Now()
	_, err = client.Send(ctx, "", []Message{{Role: RoleUser, Content: "Reply with the single word: ok"}}, nil)
	elapsed := time.Since(start)
	if err != nil {
		if ctx.Err() != nil {
			return elapsed, fmt.Errorf("no response within %s", ProbeTimeout)
		}
		msg := err.Error()
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		return elapsed, errors.New(msg)
	}
	return elapsed, nil
}

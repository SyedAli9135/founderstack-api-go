package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// httpClient is shared by every REST-based verify (OpenAI-compatible +
// Gemini). The timeout matters here specifically: this call sits in the
// request path of a founder submitting a key, and a hung third-party API
// shouldn't hang the HTTP handler indefinitely.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// verifiers maps each Catalog provider to its network-calling verify.
// Kept in one place, alongside ValidateKey's dispatch, so adding provider
// #6 is a Catalog entry plus one line here.
var verifiers = map[ProviderID]verify{
	ProviderAnthropic: verifyAnthropic,
	ProviderOpenAI:    verifyOpenAICompatible("https://api.openai.com/v1/models"),
	ProviderQwen:      verifyOpenAICompatible("https://dashscope.aliyuncs.com/compatible-mode/v1/models"),
	ProviderDeepSeek:  verifyOpenAICompatible("https://api.deepseek.com/v1/models"),
	ProviderGemini:    verifyGemini,
}

// verifyAnthropic is the real, network-calling verify — a cheap,
// side-effect-free call, same as the Python original's
// client.models.list(limit=1), just enough to know the key works.
func verifyAnthropic(ctx context.Context, apiKey string) error {
	client := newClient(apiKey)
	_, err := client.Models.List(ctx, anthropic.ModelListParams{Limit: anthropic.Int(1)})
	if err == nil {
		return nil
	}

	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == 401 {
		return fmt.Errorf("%w: %w", ErrKeyRejected, err)
	}
	return fmt.Errorf("%w: %w", ErrValidationUnavailable, err)
}

// verifyOpenAICompatible builds a verify for any provider exposing an
// OpenAI-compatible GET {modelsURL} endpoint — OpenAI, Qwen (DashScope's
// compatible-mode endpoint), and DeepSeek all mirror OpenAI's API shape.
// No SDK for a one-line REST call, matching this codebase's "don't add a
// dependency a single GET doesn't justify" policy.
func verifyOpenAICompatible(modelsURL string) verify {
	return func(ctx context.Context, apiKey string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrValidationUnavailable, err)
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)

		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrValidationUnavailable, err)
		}
		defer resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusOK:
			return nil
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return fmt.Errorf("%w: unexpected status %d", ErrKeyRejected, resp.StatusCode)
		default:
			return fmt.Errorf("%w: unexpected status %d", ErrValidationUnavailable, resp.StatusCode)
		}
	}
}

// A var, not a local const, so verify_test.go can point it at a fake
// httptest server instead of the real Google API.
var geminiModelsURL = "https://generativelanguage.googleapis.com/v1beta/models"

// verifyGemini checks a Google AI Studio key via the same "list models,
// cheap and side-effect-free" idea — Gemini authenticates via a `key`
// query parameter, not a Bearer header, which is why it isn't folded into
// verifyOpenAICompatible.
func verifyGemini(ctx context.Context, apiKey string) error {
	url := geminiModelsURL + "?key=" + apiKey
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrValidationUnavailable, err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrValidationUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest:
		// Google returns 400, not 401, for a malformed/invalid API key.
		return fmt.Errorf("%w: unexpected status %d", ErrKeyRejected, resp.StatusCode)
	default:
		return fmt.Errorf("%w: unexpected status %d", ErrValidationUnavailable, resp.StatusCode)
	}
}

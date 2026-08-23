// Package servers holds one file per MCP tool server (workflow 5) —
// Stripe, Slack, GitHub, Notion, LinkedIn. Each exposes a constructor
// (NewStripeServer, ...) returning a *mcp.Server ready for
// internal/core/mcp.NewRegistry to connect. Every tool handler reads its
// credential via mcp.TokenFromContext — never fetches or accepts one as
// a tool argument. See internal/core/mcp's package doc for why.
package servers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpClient is shared by every tool server's REST calls — same "don't
// add a dependency a single HTTP call doesn't justify" reasoning already
// applied to Stripe's ValidateKey (internal/core/integrations/providers/stripe.go)
// and the 5-provider LLM verify calls (internal/core/llm/verify.go). 15s,
// not 10s: unlike a cheap validation ping, some of these (Slack channel
// listing, GitHub code search) can legitimately take a little longer.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// newRequestWithBody builds a request, JSON-marshaling body onto it when
// non-nil. Shared by every tool server here that needs provider-specific
// headers beyond what doJSON sets (GitHub's Accept/API-Version, Notion's
// Notion-Version) — those wrap this instead of duplicating the
// marshal-and-build boilerplate.
func newRequestWithBody(ctx context.Context, method, endpoint string, body any) (*http.Request, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("servers: marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return nil, fmt.Errorf("servers: build request: %w", err)
	}
	return req, nil
}

// doJSON issues a Bearer-authed request with an optional JSON body and
// decodes a JSON response into out (a pointer, or nil to discard the
// body). Used by every tool server here except Stripe, which
// authenticates with HTTP Basic instead of a Bearer header.
func doJSON(ctx context.Context, method, endpoint, token string, body, out any) error {
	req, err := newRequestWithBody(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	return doAndDecode(req, out)
}

// doStripeForm issues a Basic-auth request (Stripe's convention: secret
// key as username, empty password) with an application/x-www-form-urlencoded
// body — Stripe's write endpoints don't accept JSON.
func doStripeForm(ctx context.Context, method, endpoint, apiKey string, form url.Values, out any) error {
	var reqBody io.Reader
	if form != nil {
		reqBody = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return fmt.Errorf("servers: build request: %w", err)
	}
	req.SetBasicAuth(apiKey, "")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return doAndDecode(req, out)
}

func doAndDecode(req *http.Request, out any) error {
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("servers: request %s: %w", req.URL, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return fmt.Errorf("servers: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("servers: %s returned %d: %s", req.URL, resp.StatusCode, truncate(respBody, 500))
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("servers: decode response: %w", err)
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}

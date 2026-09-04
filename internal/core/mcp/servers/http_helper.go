// Package servers holds one file per MCP tool server, each exposing a
// constructor (NewStripeServer, ...) returning a *mcp.Server ready for
// internal/core/mcp.NewRegistry to connect. Every handler reads its
// credential via mcp.TokenFromRequest, never as a tool argument.
package servers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	mcp "github.com/founderstack/api/internal/core/mcp"
)

// toolCallMaxAttempts bounds retry-with-backoff for a transient tool-call
// failure. Classification has to happen here, at the HTTP layer, not in
// the graph engine — MCP flattens a handler's returned error before it
// crosses the protocol boundary, so a sentinel wrapped there wouldn't
// survive.
const toolCallMaxAttempts = 3

// var, not const, so tests can shrink it to near-zero and assert
// retry-count behavior without real sleeps.
var retryBackoffBase = 300 * time.Millisecond

func sleepBackoff(ctx context.Context, attempt int) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Duration(attempt) * retryBackoffBase):
		return true
	}
}

// 15s, not the usual 10s: some calls here (Slack channel listing, GitHub
// code search) can legitimately take longer than a cheap validation ping.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// newRequestWithBody is wrapped by servers needing headers beyond what
// doJSON sets (GitHub's Accept/API-Version, Notion's Notion-Version).
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

// doJSON is used by every server except Stripe, which authenticates via
// HTTP Basic instead of a Bearer header.
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

// doStripeForm uses Basic auth (secret key as username, empty password)
// and form-encoded bodies — Stripe's write endpoints don't accept JSON.
// idempotencyKey, when non-empty, is forwarded as Stripe's native
// Idempotency-Key header, which is what actually protects
// stripeCreateInvoice/stripeRefundPayment from double-executing on a
// retried call (approval Resume, crash replay). Read-only calls pass "".
func doStripeForm(ctx context.Context, method, endpoint, apiKey, idempotencyKey string, form url.Values, out any) error {
	var reqBody io.Reader
	if form != nil {
		reqBody = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return fmt.Errorf("servers: build request: %w", err)
	}
	req.SetBasicAuth(apiKey, "")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return doAndDecode(req, out)
}

func doAndDecode(req *http.Request, out any) error {
	// A network-level failure is ambiguous for a write (the request may
	// have already landed before the response was lost), so only GET
	// retries on that; both retry on an explicit 429/5xx response, since
	// that means the server rejected the request without applying it.
	isWrite := req.Method != http.MethodGet

	var lastErr error
	for attempt := 1; attempt <= toolCallMaxAttempts; attempt++ {
		if attempt > 1 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return fmt.Errorf("servers: rebuild request body for retry: %w", err)
			}
			req.Body = body
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			wrapped := fmt.Errorf("%w: %s: %w", mcp.ErrToolRetryable, req.URL, err)
			if isWrite {
				return wrapped
			}
			lastErr = wrapped
			if attempt < toolCallMaxAttempts && sleepBackoff(req.Context(), attempt) {
				continue
			}
			return lastErr
		}

		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
		resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("servers: read response: %w", readErr)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			classified := mcp.ClassifyToolHTTPError(resp.StatusCode, fmt.Sprintf("%s returned: %s", req.URL, truncate(respBody, 500)))
			if errors.Is(classified, mcp.ErrToolRetryable) && attempt < toolCallMaxAttempts && sleepBackoff(req.Context(), attempt) {
				lastErr = classified
				continue
			}
			return classified
		}

		if out == nil || len(respBody) == 0 {
			return nil
		}
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("servers: decode response: %w", err)
		}
		return nil
	}
	return lastErr
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}

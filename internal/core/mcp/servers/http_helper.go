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
// failure (rate limit, server error, network hiccup) — see
// internal/core/mcp.ErrToolRetryable's doc comment for why this has to
// live here, at the HTTP-call layer, rather than in the graph engine.
const toolCallMaxAttempts = 3

// retryBackoffBase is a var, not a const, purely so tests can shrink it
// to near-zero and assert retry-count behavior without real sleeps —
// same test-seam pattern as stripeAPIBase/geminiModelsURL elsewhere in
// this codebase.
var retryBackoffBase = 300 * time.Millisecond

// sleepBackoff waits attempt*retryBackoffBase before the next retry, or
// returns false immediately if ctx is done — callers treat false as
// "give up, don't retry."
func sleepBackoff(ctx context.Context, attempt int) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Duration(attempt) * retryBackoffBase):
		return true
	}
}

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
// body — Stripe's write endpoints don't accept JSON. idempotencyKey, when
// non-empty, is forwarded as Stripe's own native `Idempotency-Key` header
// (https://docs.stripe.com/api/idempotent_requests) — Stripe recognizes a
// retried request carrying the same key and returns the original result
// instead of re-applying the effect, which is what actually protects
// stripeCreateInvoice/stripeRefundPayment from double-executing if the
// same logical tool call is ever retried (an approval batch
// re-`Resume()`'d, a crash-recovery replay — see
// internal/core/graph/nodes.go's idempotency-key computation, and
// mcp.WithIdempotencyKey/IdempotencyKeyFromRequest for how the key
// arrives here via `_meta`). Read-only calls pass "".
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
	// A network-level failure (httpClient.Do itself erroring) is
	// ambiguous for a write: the request may have already landed
	// server-side before the response was lost, so blindly retrying
	// could double-execute a create/refund/delete. Only GET calls retry
	// on that failure mode; a write's network error surfaces immediately
	// as ErrToolRetryable (still classified correctly, just not
	// auto-retried here) rather than being retried blind. An explicit
	// 429/5xx *response* is unambiguous either way — the server rejected
	// the request outright without applying it — so both reads and
	// writes retry on that.
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

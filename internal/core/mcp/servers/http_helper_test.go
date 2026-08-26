package servers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	mcp "github.com/founderstack/api/internal/core/mcp"
)

func init() {
	// These tests assert retry *counts*, not real timing — shrink the
	// backoff so the suite doesn't pay hundreds of ms per retry test.
	retryBackoffBase = time.Millisecond
}

// flakyTransport is a fake http.RoundTripper (never hits a real network
// or httptest listener) that fails with a network-level error for its
// first failNetworkCalls invocations, then returns the given sequence of
// status codes (the last one repeating for any call beyond the sequence).
type flakyTransport struct {
	calls            int
	failNetworkCalls int
	statusCodes      []int
}

func (t *flakyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls++
	if t.calls <= t.failNetworkCalls {
		return nil, errors.New("simulated network failure")
	}
	idx := t.calls - t.failNetworkCalls - 1
	status := http.StatusOK
	if len(t.statusCodes) > 0 {
		if idx < len(t.statusCodes) {
			status = t.statusCodes[idx]
		} else {
			status = t.statusCodes[len(t.statusCodes)-1]
		}
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Header:     make(http.Header),
	}, nil
}

// withFlakyTransport swaps the package's shared httpClient's Transport
// for the test's duration and restores it afterward — same "swap a
// package var, restore via t.Cleanup" pattern this codebase already uses
// for stripeAPIBase/geminiModelsURL-style test seams.
func withFlakyTransport(t *testing.T, ft *flakyTransport) {
	t.Helper()
	orig := httpClient.Transport
	httpClient.Transport = ft
	t.Cleanup(func() { httpClient.Transport = orig })
}

func newTestRequest(t *testing.T, method string) *http.Request {
	t.Helper()
	var body io.Reader
	if method != http.MethodGet {
		body = bytes.NewReader([]byte(`{"x":1}`)) // *bytes.Reader auto-populates req.GetBody
	}
	req, err := http.NewRequestWithContext(context.Background(), method, "http://fake.example/x", body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestDoAndDecode_RetriesOnRetryableStatusThenSucceeds(t *testing.T) {
	ft := &flakyTransport{statusCodes: []int{429, 200}}
	withFlakyTransport(t, ft)

	if err := doAndDecode(newTestRequest(t, http.MethodGet), nil); err != nil {
		t.Fatalf("doAndDecode() error = %v, want nil after retry succeeds", err)
	}
	if ft.calls != 2 {
		t.Fatalf("calls = %d, want 2 (1 retry after the 429)", ft.calls)
	}
}

func TestDoAndDecode_DoesNotRetryTerminalStatus(t *testing.T) {
	ft := &flakyTransport{statusCodes: []int{400}}
	withFlakyTransport(t, ft)

	err := doAndDecode(newTestRequest(t, http.MethodGet), nil)
	if !errors.Is(err, mcp.ErrToolTerminal) {
		t.Fatalf("err = %v, want ErrToolTerminal", err)
	}
	if ft.calls != 1 {
		t.Fatalf("calls = %d, want 1 (a terminal status must not retry)", ft.calls)
	}
}

func TestDoAndDecode_GETRetriesOnNetworkError(t *testing.T) {
	ft := &flakyTransport{failNetworkCalls: 1, statusCodes: []int{200}}
	withFlakyTransport(t, ft)

	if err := doAndDecode(newTestRequest(t, http.MethodGet), nil); err != nil {
		t.Fatalf("doAndDecode() error = %v, want nil after retry succeeds", err)
	}
	if ft.calls != 2 {
		t.Fatalf("calls = %d, want 2 (a GET's network error is safe to retry)", ft.calls)
	}
}

func TestDoAndDecode_WriteDoesNotRetryNetworkError(t *testing.T) {
	// A write's network-level failure is ambiguous (the request may have
	// already landed server-side) — must never auto-retry, unlike a GET's.
	ft := &flakyTransport{failNetworkCalls: 5, statusCodes: []int{200}}
	withFlakyTransport(t, ft)

	err := doAndDecode(newTestRequest(t, http.MethodPost), nil)
	if !errors.Is(err, mcp.ErrToolRetryable) {
		t.Fatalf("err = %v, want it still classified as ErrToolRetryable (just not auto-retried)", err)
	}
	if ft.calls != 1 {
		t.Fatalf("calls = %d, want exactly 1 (a write must not retry on a network-level failure)", ft.calls)
	}
}

func TestDoAndDecode_WriteRetriesOnExplicitServerError(t *testing.T) {
	// Unlike a network-level failure, an explicit 5xx *response* means
	// the server rejected the request outright without applying it —
	// safe to retry even for a write.
	ft := &flakyTransport{statusCodes: []int{503, 200}}
	withFlakyTransport(t, ft)

	if err := doAndDecode(newTestRequest(t, http.MethodPost), nil); err != nil {
		t.Fatalf("doAndDecode() error = %v, want nil after retry succeeds", err)
	}
	if ft.calls != 2 {
		t.Fatalf("calls = %d, want 2 (an explicit 503 on a write is safe to retry)", ft.calls)
	}
}

func TestDoAndDecode_GivesUpAfterMaxAttempts(t *testing.T) {
	ft := &flakyTransport{statusCodes: []int{500, 500, 500, 500, 500}}
	withFlakyTransport(t, ft)

	err := doAndDecode(newTestRequest(t, http.MethodGet), nil)
	if !errors.Is(err, mcp.ErrToolRetryable) {
		t.Fatalf("err = %v, want ErrToolRetryable", err)
	}
	if ft.calls != toolCallMaxAttempts {
		t.Fatalf("calls = %d, want exactly %d (bounded retry, not unbounded)", ft.calls, toolCallMaxAttempts)
	}
}

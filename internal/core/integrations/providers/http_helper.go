package providers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// httpClient is shared by every provider's plain REST calls (validate,
// revoke, key-check) — a 10s timeout so a slow/hung third party can't
// stall a request indefinitely; none of these calls are expected to take
// more than a second or two in practice.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// bearerRequest issues method against url with an "Authorization: Bearer
// token" header and returns an error unless the response status is 2xx —
// the shared shape behind several providers' ValidateToken/RevokeToken,
// none of which need to inspect a response body beyond that.
func bearerRequest(ctx context.Context, method, url, token string) error {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return fmt.Errorf("providers: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return doAndCheck(req)
}

// simpleRequest is bearerRequest without the Authorization header — for
// the handful of endpoints (Google's revoke/tokeninfo) that take the
// token as a query parameter instead of a Bearer header.
func simpleRequest(ctx context.Context, method, url string) error {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return fmt.Errorf("providers: build request: %w", err)
	}
	return doAndCheck(req)
}

// basicAuthRequest is bearerRequest's HTTP-Basic-auth counterpart —
// Stripe's API authenticates this way (secret key as username, empty
// password) rather than with a Bearer header.
func basicAuthRequest(ctx context.Context, method, url, username, password string) error {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return fmt.Errorf("providers: build request: %w", err)
	}
	req.SetBasicAuth(username, password)
	return doAndCheck(req)
}

func doAndCheck(req *http.Request) error {
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("providers: request %s: %w", req.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("providers: %s returned %d: %s", req.URL, resp.StatusCode, body)
	}
	return nil
}

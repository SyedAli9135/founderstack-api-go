package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"golang.org/x/oauth2"
)

// hostInterceptor reroutes requests for specific hostnames to local
// httptest servers, keeping method/path/query/body otherwise intact —
// lets ExchangeCode/RefreshAccessToken/RevokeToken/ValidateToken run
// against a fake server standing in for the real Slack/Google/etc.
// endpoint, without either hardcoding test URLs into the provider files
// or depending on a live third party being reachable in CI. This is the
// same "don't depend on live third-party availability" reasoning as
// llm.go's ValidateKey doc comment — it just achieves it via interception
// instead of skipping the test outright, since the request/response
// shape (JSON fields, header names, the "ok" field Slack always 200s
// with) is exactly the kind of "not obviously right" logic worth locking
// down with a real HTTP round trip.
type hostInterceptor struct {
	routes map[string]*httptest.Server
}

func (i *hostInterceptor) RoundTrip(req *http.Request) (*http.Response, error) {
	srv, ok := i.routes[req.URL.Host]
	if !ok {
		return nil, &url.Error{Op: "RoundTrip", URL: req.URL.String(), Err: errUnroutedHost}
	}
	target, err := url.Parse(srv.URL)
	if err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	return http.DefaultTransport.RoundTrip(req)
}

var errUnroutedHost = &unroutedHostError{}

type unroutedHostError struct{}

func (e *unroutedHostError) Error() string {
	return "hostInterceptor: no test server registered for this host"
}

// withIntercept swaps the package-level httpClient (used directly by
// Slack, and indirectly by every ValidateToken/RevokeToken built on
// bearerRequest/simpleRequest/basicAuthRequest) for the duration of the
// test, and returns a context carrying the same client for
// golang.org/x/oauth2 calls (Exchange/TokenSource respect
// oauth2.HTTPClient on the context, not our package var).
func withIntercept(t *testing.T, routes map[string]*httptest.Server) context.Context {
	t.Helper()
	client := &http.Client{Transport: &hostInterceptor{routes: routes}}

	origHTTPClient := httpClient
	httpClient = client
	t.Cleanup(func() { httpClient = origHTTPClient })

	for _, srv := range routes {
		t.Cleanup(srv.Close)
	}
	return context.WithValue(context.Background(), oauth2.HTTPClient, client)
}

func jsonServer(t *testing.T, status int, body any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func TestSlack_ExchangeCode(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		srv := jsonServer(t, http.StatusOK, map[string]any{"ok": true, "access_token": "xoxb-real"})
		ctx := withIntercept(t, map[string]*httptest.Server{"slack.com": srv})

		s := NewSlack("cid", "csecret", "https://api.example.com/cb")
		tok, err := s.ExchangeCode(ctx, "code123")
		if err != nil {
			t.Fatalf("ExchangeCode: %v", err)
		}
		if tok.AccessToken != "xoxb-real" {
			t.Fatalf("access token = %q, want xoxb-real", tok.AccessToken)
		}
	})

	t.Run("slack ok:false is an error despite HTTP 200", func(t *testing.T) {
		srv := jsonServer(t, http.StatusOK, map[string]any{"ok": false, "error": "invalid_code"})
		ctx := withIntercept(t, map[string]*httptest.Server{"slack.com": srv})

		s := NewSlack("cid", "csecret", "https://api.example.com/cb")
		_, err := s.ExchangeCode(ctx, "bad-code")
		if err == nil {
			t.Fatal("got nil error for ok:false response, want an error")
		}
	})
}

func TestSlack_RevokeAndValidate(t *testing.T) {
	srv := jsonServer(t, http.StatusOK, map[string]any{"ok": true})
	ctx := withIntercept(t, map[string]*httptest.Server{"slack.com": srv})
	s := NewSlack("cid", "csecret", "https://api.example.com/cb")

	if err := s.RevokeToken(ctx, "xoxb-token"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if err := s.ValidateToken(ctx, "xoxb-token"); err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
}

func TestSlack_ValidateToken_NotOK(t *testing.T) {
	srv := jsonServer(t, http.StatusOK, map[string]any{"ok": false, "error": "invalid_auth"})
	ctx := withIntercept(t, map[string]*httptest.Server{"slack.com": srv})
	s := NewSlack("cid", "csecret", "https://api.example.com/cb")

	if err := s.ValidateToken(ctx, "bad-token"); err == nil {
		t.Fatal("got nil error for ok:false, want an error")
	}
}

func TestDiscord_ExchangeRevokeValidate(t *testing.T) {
	d := NewDiscord("cid", "csecret", "https://api.example.com/cb")

	// Exchange, revoke, and validate each hit a different path on the same
	// production host (discord.com) — one stub server per call, swapped in
	// via a fresh withIntercept each time, since a single httptest.Server
	// only serves one canned response shape.
	tokenSrv := jsonServer(t, http.StatusOK, map[string]any{
		"access_token": "discord-access", "refresh_token": "discord-refresh", "token_type": "Bearer", "expires_in": 3600,
	})
	ctx := withIntercept(t, map[string]*httptest.Server{"discord.com": tokenSrv})
	tok, err := d.ExchangeCode(ctx, "code")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok.AccessToken != "discord-access" || tok.RefreshToken != "discord-refresh" {
		t.Fatalf("got token %+v", tok)
	}

	revokeSrv := jsonServer(t, http.StatusOK, map[string]any{})
	ctx = withIntercept(t, map[string]*httptest.Server{"discord.com": revokeSrv})
	if err := d.RevokeToken(ctx, "discord-access"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	meSrv := jsonServer(t, http.StatusOK, map[string]any{"id": "123"})
	ctx = withIntercept(t, map[string]*httptest.Server{"discord.com": meSrv})
	if err := d.ValidateToken(ctx, "discord-access"); err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
}

// TestDiscord_ExchangeCode_CapturesWebhook proves the webhook.incoming
// grant's webhook object survives ExchangeCode into Token.Extra — a real
// bug (silently discarded) until workflow 5's Discord MCP tool needed it.
func TestDiscord_ExchangeCode_CapturesWebhook(t *testing.T) {
	d := NewDiscord("cid", "csecret", "https://api.example.com/cb")

	tokenSrv := jsonServer(t, http.StatusOK, map[string]any{
		"access_token": "discord-access", "refresh_token": "discord-refresh",
		"token_type": "Bearer", "expires_in": 3600,
		"webhook": map[string]any{
			"id":         "223704706495545344",
			"token":      "3d89bb7572e0fb30d8128367b3b1b44",
			"channel_id": "223704706495545399",
			"url":        "https://discord.com/api/webhooks/223704706495545344/3d89bb7572e0fb30d8128367b3b1b44",
		},
	})
	ctx := withIntercept(t, map[string]*httptest.Server{"discord.com": tokenSrv})

	tok, err := d.ExchangeCode(ctx, "code")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	want := map[string]string{
		"webhook_id":         "223704706495545344",
		"webhook_token":      "3d89bb7572e0fb30d8128367b3b1b44",
		"webhook_channel_id": "223704706495545399",
		"webhook_url":        "https://discord.com/api/webhooks/223704706495545344/3d89bb7572e0fb30d8128367b3b1b44",
	}
	for k, v := range want {
		if tok.Extra[k] != v {
			t.Errorf("Extra[%q] = %q, want %q", k, tok.Extra[k], v)
		}
	}
}

// TestDiscord_ExchangeCode_NoWebhookIsFine proves a grant without the
// webhook object (e.g. identify-only) doesn't error or panic — Extra
// just stays nil.
func TestDiscord_ExchangeCode_NoWebhookIsFine(t *testing.T) {
	d := NewDiscord("cid", "csecret", "https://api.example.com/cb")

	tokenSrv := jsonServer(t, http.StatusOK, map[string]any{
		"access_token": "discord-access", "token_type": "Bearer", "expires_in": 3600,
	})
	ctx := withIntercept(t, map[string]*httptest.Server{"discord.com": tokenSrv})

	tok, err := d.ExchangeCode(ctx, "code")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if len(tok.Extra) != 0 {
		t.Errorf("Extra = %+v, want empty", tok.Extra)
	}
}

func TestNotion_ExchangeAndValidate(t *testing.T) {
	tokenSrv := jsonServer(t, http.StatusOK, map[string]any{"access_token": "notion-access", "token_type": "bearer"})
	ctx := withIntercept(t, map[string]*httptest.Server{"api.notion.com": tokenSrv})

	n := NewNotion("cid", "csecret", "https://api.example.com/cb")
	tok, err := n.ExchangeCode(ctx, "code")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok.AccessToken != "notion-access" {
		t.Fatalf("access token = %q, want notion-access", tok.AccessToken)
	}

	validateSrv := jsonServer(t, http.StatusOK, map[string]any{"id": "user-1"})
	ctx = withIntercept(t, map[string]*httptest.Server{"api.notion.com": validateSrv})
	if err := n.ValidateToken(ctx, "notion-access"); err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
}

func TestGoogleDrive_ExchangeRefreshRevokeValidate(t *testing.T) {
	tokenSrv := jsonServer(t, http.StatusOK, map[string]any{
		"access_token": "g-access", "refresh_token": "g-refresh", "token_type": "Bearer", "expires_in": 3600,
	})
	ctx := withIntercept(t, map[string]*httptest.Server{"oauth2.googleapis.com": tokenSrv})

	g := NewGoogleDrive("cid", "csecret", "https://api.example.com/cb")
	tok, err := g.ExchangeCode(ctx, "code")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok.AccessToken != "g-access" {
		t.Fatalf("access token = %q, want g-access", tok.AccessToken)
	}

	refreshed, err := g.RefreshAccessToken(ctx, "g-refresh")
	if err != nil {
		t.Fatalf("RefreshAccessToken: %v", err)
	}
	if refreshed.AccessToken != "g-access" {
		t.Fatalf("refreshed access token = %q, want g-access", refreshed.AccessToken)
	}

	revokeSrv := jsonServer(t, http.StatusOK, map[string]any{})
	ctx = withIntercept(t, map[string]*httptest.Server{"oauth2.googleapis.com": revokeSrv})
	if err := g.RevokeToken(ctx, "g-access"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	validateSrv := jsonServer(t, http.StatusOK, map[string]any{"scope": "drive.file"})
	ctx = withIntercept(t, map[string]*httptest.Server{"www.googleapis.com": validateSrv})
	if err := g.ValidateToken(ctx, "g-access"); err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
}

func TestGoogleCalendar_ExchangeAndValidate(t *testing.T) {
	tokenSrv := jsonServer(t, http.StatusOK, map[string]any{
		"access_token": "gc-access", "refresh_token": "gc-refresh", "token_type": "Bearer", "expires_in": 3600,
	})
	ctx := withIntercept(t, map[string]*httptest.Server{"oauth2.googleapis.com": tokenSrv})

	g := NewGoogleCalendar("cid", "csecret", "https://api.example.com/cb")
	tok, err := g.ExchangeCode(ctx, "code")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok.AccessToken != "gc-access" {
		t.Fatalf("access token = %q, want gc-access", tok.AccessToken)
	}

	validateSrv := jsonServer(t, http.StatusOK, map[string]any{"scope": "calendar.events"})
	ctx = withIntercept(t, map[string]*httptest.Server{"www.googleapis.com": validateSrv})
	if err := g.ValidateToken(ctx, "gc-access"); err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
}

func TestLinkedIn_ExchangeRevokeValidate(t *testing.T) {
	tokenSrv := jsonServer(t, http.StatusOK, map[string]any{"access_token": "li-access", "expires_in": 5184000})
	ctx := withIntercept(t, map[string]*httptest.Server{"www.linkedin.com": tokenSrv})

	l := NewLinkedIn("cid", "csecret", "https://api.example.com/cb")
	tok, err := l.ExchangeCode(ctx, "code")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok.AccessToken != "li-access" {
		t.Fatalf("access token = %q, want li-access", tok.AccessToken)
	}

	revokeSrv := jsonServer(t, http.StatusOK, map[string]any{})
	ctx = withIntercept(t, map[string]*httptest.Server{"www.linkedin.com": revokeSrv})
	if err := l.RevokeToken(ctx, "li-access"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	activeSrv := jsonServer(t, http.StatusOK, map[string]any{"active": true})
	ctx = withIntercept(t, map[string]*httptest.Server{"www.linkedin.com": activeSrv})
	if err := l.ValidateToken(ctx, "li-access"); err != nil {
		t.Fatalf("ValidateToken (active): %v", err)
	}

	inactiveSrv := jsonServer(t, http.StatusOK, map[string]any{"active": false})
	ctx = withIntercept(t, map[string]*httptest.Server{"www.linkedin.com": inactiveSrv})
	if err := l.ValidateToken(ctx, "li-access"); err == nil {
		t.Fatal("got nil error for active:false, want an error")
	}
}

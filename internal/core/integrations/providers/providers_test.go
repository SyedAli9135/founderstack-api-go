package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetAuthURL_EmbedsStateAndClientID(t *testing.T) {
	cases := []struct {
		name string
		auth interface {
			GetAuthURL(state string) string
		}
		wantHost string
	}{
		{"slack", NewSlack("client-1", "secret", "https://api.example.com/cb"), "slack.com"},
		{"discord", NewDiscord("client-1", "secret", "https://api.example.com/cb"), "discord.com"},
		{"notion", NewNotion("client-1", "secret", "https://api.example.com/cb"), "api.notion.com"},
		{"google_drive", NewGoogleDrive("client-1", "secret", "https://api.example.com/cb"), "accounts.google.com"},
		{"google_calendar", NewGoogleCalendar("client-1", "secret", "https://api.example.com/cb"), "accounts.google.com"},
		{"linkedin", NewLinkedIn("client-1", "secret", "https://api.example.com/cb"), "linkedin.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := tc.auth.GetAuthURL("the-state-value")
			if !strings.Contains(url, "the-state-value") {
				t.Errorf("auth URL %q doesn't embed the state value", url)
			}
			if !strings.Contains(url, "client-1") {
				t.Errorf("auth URL %q doesn't embed the client ID", url)
			}
			if !strings.Contains(url, tc.wantHost) {
				t.Errorf("auth URL %q doesn't point at %s", url, tc.wantHost)
			}
		})
	}
}

// TestBearerRequest covers the shared helper backing GitHub.ValidateKey,
// Discord.ValidateToken, and Discord/LinkedIn's revoke calls — GitHub's
// own ValidateKey has its endpoint hardcoded, so this exercises the same
// logic against a stub instead.
func TestBearerRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := bearerRequest(context.Background(), "GET", srv.URL, "good-token"); err != nil {
		t.Fatalf("bearerRequest with correct token: %v", err)
	}
	if err := bearerRequest(context.Background(), "GET", srv.URL, "bad-token"); err == nil {
		t.Fatal("bearerRequest with wrong token: got nil error, want one")
	}
}

func TestSimpleRequest(t *testing.T) {
	// simpleRequest is bearerRequest's no-Authorization-header counterpart,
	// used by Google Drive/Calendar's revoke and tokeninfo calls, where the
	// token travels as a query/path value instead of a header.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/good-token/") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := simpleRequest(context.Background(), "GET", srv.URL+"/good-token/check"); err != nil {
		t.Fatalf("simpleRequest with correct path value: %v", err)
	}
	if err := simpleRequest(context.Background(), "GET", srv.URL+"/bad-token/check"); err == nil {
		t.Fatal("simpleRequest with wrong path value: got nil error, want one")
	}
}

func TestStripe_ValidateKey_UsesBasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "sk_test_good" || pass != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := basicAuthRequest(context.Background(), "GET", srv.URL, "sk_test_good", ""); err != nil {
		t.Fatalf("basicAuthRequest with correct key: %v", err)
	}
	if err := basicAuthRequest(context.Background(), "GET", srv.URL, "sk_test_bad", ""); err == nil {
		t.Fatal("basicAuthRequest with wrong key: got nil error, want one")
	}
}

//go:build integration

package integrations

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"
)

// fakeRefreshableProvider is a minimal OAuthProvider + Refreshable used
// only to drive refreshExpiringConnections without depending on a live
// third-party token endpoint — same reasoning as the fake providers in
// internal/api/integrations's handler_integration_test.go.
type fakeRefreshableProvider struct {
	name        string
	refreshedTo *Token
	refreshErr  error
	calls       int
}

func (f *fakeRefreshableProvider) Name() string             { return f.name }
func (f *fakeRefreshableProvider) GetAuthURL(string) string { return "" }
func (f *fakeRefreshableProvider) ExchangeCode(context.Context, string, url.Values) (*Token, error) {
	return nil, errors.New("not used by this test")
}
func (f *fakeRefreshableProvider) RefreshAccessToken(ctx context.Context, refreshToken string) (*Token, error) {
	f.calls++
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	return f.refreshedTo, nil
}

func TestRefreshExpiringConnections_SuccessfulRefresh(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	encKey := testEncryptionKey(t)
	orgID := testOrg(t, systemPool)
	ctx := context.Background()

	// Save a connection that's already expiring within the refresh window,
	// with a refresh token and an Extra field that must survive the
	// refresh (the fake refresh response deliberately omits it, same as a
	// real provider's refresh response never re-sends provider-specific
	// extras) — a fake provider drives this, not any real one, so the
	// service name/Extra key here are arbitrary, not tied to any
	// particular provider's convention.
	original := Token{
		AccessToken:  "old-access-token",
		RefreshToken: "the-refresh-token",
		ExpiresAt:    time.Now().Add(1 * time.Minute), // inside refreshWindow
		Extra:        map[string]string{"some_extra_field": "12345"},
	}
	if err := SaveConnection(ctx, appPool, encKey, orgID, "fake-refreshable", "Fake Refreshable Service", "oauth", "connected", original); err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}

	newExpiry := time.Now().Add(time.Hour).UTC().Round(time.Second)
	fake := &fakeRefreshableProvider{
		name:        "fake-refreshable",
		refreshedTo: &Token{AccessToken: "new-access-token", RefreshToken: "the-refresh-token", ExpiresAt: newExpiry},
	}
	registry := NewRegistry(fake)

	refreshExpiringConnections(ctx, systemPool, encKey, registry)

	if fake.calls != 1 {
		t.Fatalf("RefreshAccessToken called %d times, want 1", fake.calls)
	}

	conn, err := GetConnection(ctx, appPool, encKey, orgID, "fake-refreshable")
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	if conn.Token.AccessToken != "new-access-token" {
		t.Fatalf("access token = %q, want new-access-token", conn.Token.AccessToken)
	}
	if !conn.Token.ExpiresAt.Equal(newExpiry) {
		t.Fatalf("expires_at = %v, want %v", conn.Token.ExpiresAt, newExpiry)
	}
	if conn.Token.Extra["some_extra_field"] != "12345" {
		t.Fatalf("extra field = %q, want it preserved as 12345", conn.Token.Extra["some_extra_field"])
	}
	if conn.OAuthStatus != "connected" {
		t.Fatalf("oauth_status = %s, want connected", conn.OAuthStatus)
	}
}

func TestRefreshExpiringConnections_FailedRefreshMarksExpired(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	encKey := testEncryptionKey(t)
	orgID := testOrg(t, systemPool)
	ctx := context.Background()

	original := Token{
		AccessToken:  "old-access-token",
		RefreshToken: "a-now-invalid-refresh-token",
		ExpiresAt:    time.Now().Add(1 * time.Minute),
	}
	if err := SaveConnection(ctx, appPool, encKey, orgID, "google_drive", "Google Drive", "oauth", "connected", original); err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}

	fake := &fakeRefreshableProvider{name: "google_drive", refreshErr: errors.New("invalid_grant")}
	registry := NewRegistry(fake)

	refreshExpiringConnections(ctx, systemPool, encKey, registry)

	conn, err := GetConnection(ctx, appPool, encKey, orgID, "google_drive")
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	if conn.OAuthStatus != "expired" {
		t.Fatalf("oauth_status = %s, want expired", conn.OAuthStatus)
	}
}

func TestRefreshExpiringConnections_IgnoresConnectionsNotYetExpiring(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	encKey := testEncryptionKey(t)
	orgID := testOrg(t, systemPool)
	ctx := context.Background()

	farFuture := Token{
		AccessToken:  "still-good",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(24 * time.Hour), // well outside refreshWindow
	}
	if err := SaveConnection(ctx, appPool, encKey, orgID, "google_calendar", "Google Calendar", "oauth", "connected", farFuture); err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}

	fake := &fakeRefreshableProvider{name: "google_calendar", refreshedTo: &Token{AccessToken: "should-not-be-called"}}
	registry := NewRegistry(fake)

	refreshExpiringConnections(ctx, systemPool, encKey, registry)

	if fake.calls != 0 {
		t.Fatalf("RefreshAccessToken called %d times, want 0 — token isn't expiring soon", fake.calls)
	}
}

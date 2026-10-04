//go:build integration

package integrations

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/url"
	"sync"
	"sync/atomic"
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

	fake := &fakeRefreshableProvider{name: "google_drive", refreshErr: permanentRefreshErr()}
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

func permanentRefreshErr() error { return retrieveErr(400, "invalid_grant") }

// racingRefresher is goroutine-safe and slow enough that unsynchronised
// callers would genuinely overlap.
type racingRefresher struct {
	name  string
	calls atomic.Int32
	err   error
}

func (r *racingRefresher) Name() string             { return r.name }
func (r *racingRefresher) GetAuthURL(string) string { return "" }
func (r *racingRefresher) ExchangeCode(context.Context, string, url.Values) (*Token, error) {
	return nil, errors.New("unused")
}
func (r *racingRefresher) RefreshAccessToken(ctx context.Context, refreshToken string) (*Token, error) {
	r.calls.Add(1)
	time.Sleep(150 * time.Millisecond)
	if r.err != nil {
		return nil, r.err
	}
	return &Token{AccessToken: "fresh-" + fmt.Sprint(time.Now().UnixNano()), RefreshToken: refreshToken, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func expiringConnection(t *testing.T, appPool *pgxpool.Pool, key []byte, orgID pgtype.UUID, service string, expiresIn time.Duration) {
	t.Helper()
	if err := SaveConnection(context.Background(), appPool, key, orgID, service, service, "oauth", "connected", Token{
		AccessToken: "stored-access", RefreshToken: "the-refresh-token", ExpiresAt: time.Now().Add(expiresIn),
	}); err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}
}

func TestRefreshExpiringConnections_ATemporaryProviderFailureDoesNotExpireTheConnection(t *testing.T) {
	systemPool, appPool, encKey, orgID := testSystemPool(t), testAppPool(t), testEncryptionKey(t), (pgtype.UUID{})
	orgID = testOrg(t, systemPool)
	expiringConnection(t, appPool, encKey, orgID, "fake-refreshable", time.Minute)

	fake := &fakeRefreshableProvider{name: "fake-refreshable", refreshErr: retrieveErr(503, "")}
	refreshExpiringConnections(context.Background(), systemPool, encKey, NewRegistry(fake))

	conn, err := GetConnection(context.Background(), appPool, encKey, orgID, "fake-refreshable")
	if err != nil {
		t.Fatal(err)
	}
	if conn.OAuthStatus != "connected" {
		t.Fatalf("oauth_status = %s after a provider 503, want connected (a retry will fix it; a reconnect shouldn't be needed)", conn.OAuthStatus)
	}
}

func TestRefreshExpiringConnections_SkipsDeactivatedOrganizations(t *testing.T) {
	systemPool, appPool, encKey := testSystemPool(t), testAppPool(t), testEncryptionKey(t)
	orgID := testOrg(t, systemPool)
	expiringConnection(t, appPool, encKey, orgID, "fake-refreshable", time.Minute)
	if _, err := systemPool.Exec(context.Background(), "update organizations set is_active = false where id = $1", orgID); err != nil {
		t.Fatal(err)
	}

	fake := &racingRefresher{name: "fake-refreshable"}
	refreshExpiringConnections(context.Background(), systemPool, encKey, NewRegistry(fake))
	if n := fake.calls.Load(); n != 0 {
		t.Fatalf("a deactivated workspace's token was refreshed %d times, want 0", n)
	}
}

func TestRefreshConnection_SeveralInstancesRefreshOnce(t *testing.T) {
	systemPool, appPool, encKey := testSystemPool(t), testAppPool(t), testEncryptionKey(t)
	orgID := testOrg(t, systemPool)
	expiringConnection(t, appPool, encKey, orgID, "fake-refreshable", time.Minute)
	var id pgtype.UUID
	if err := systemPool.QueryRow(context.Background(), "select id from mcp_connections where org_id = $1", orgID).Scan(&id); err != nil {
		t.Fatal(err)
	}

	fake := &racingRefresher{name: "fake-refreshable"}
	registry := NewRegistry(fake)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refreshConnection(context.Background(), systemPool, encKey, registry, id)
		}()
	}
	wg.Wait()
	if n := fake.calls.Load(); n != 1 {
		t.Fatalf("the refresh token was presented %d times by 6 concurrent instances, want 1", n)
	}
}

func TestGetFreshIntegrationToken(t *testing.T) {
	systemPool, appPool, encKey := testSystemPool(t), testAppPool(t), testEncryptionKey(t)
	ctx := context.Background()
	status := func(orgID pgtype.UUID) string {
		conn, err := GetConnection(ctx, appPool, encKey, orgID, "fake-refreshable")
		if err != nil {
			t.Fatal(err)
		}
		return conn.OAuthStatus
	}

	t.Run("an unexpired token is used as is", func(t *testing.T) {
		orgID := testOrg(t, systemPool)
		expiringConnection(t, appPool, encKey, orgID, "fake-refreshable", time.Hour)
		fake := &racingRefresher{name: "fake-refreshable"}
		tok, err := GetFreshIntegrationToken(ctx, appPool, encKey, NewRegistry(fake), orgID, "fake-refreshable")
		if err != nil || tok.AccessToken != "stored-access" || fake.calls.Load() != 0 {
			t.Fatalf("got (%q, %v) with %d refreshes, want the stored token and none", tok.AccessToken, err, fake.calls.Load())
		}
	})

	t.Run("an expired token is refreshed first, and the new one is stored", func(t *testing.T) {
		orgID := testOrg(t, systemPool)
		expiringConnection(t, appPool, encKey, orgID, "fake-refreshable", -time.Minute)
		fake := &racingRefresher{name: "fake-refreshable"}
		tok, err := GetFreshIntegrationToken(ctx, appPool, encKey, NewRegistry(fake), orgID, "fake-refreshable")
		if err != nil || tok.AccessToken == "stored-access" || fake.calls.Load() != 1 {
			t.Fatalf("got (%q, %v) with %d refreshes, want a refreshed token", tok.AccessToken, err, fake.calls.Load())
		}
		stored, _ := GetConnection(ctx, appPool, encKey, orgID, "fake-refreshable")
		if stored.Token.AccessToken != tok.AccessToken {
			t.Fatal("the refreshed token wasn't persisted")
		}
	})

	t.Run("concurrent tool calls refresh once", func(t *testing.T) {
		orgID := testOrg(t, systemPool)
		expiringConnection(t, appPool, encKey, orgID, "fake-refreshable", time.Second)
		fake := &racingRefresher{name: "fake-refreshable"}
		registry := NewRegistry(fake)
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := GetFreshIntegrationToken(ctx, appPool, encKey, registry, orgID, "fake-refreshable"); err != nil {
					t.Errorf("GetFreshIntegrationToken: %v", err)
				}
			}()
		}
		wg.Wait()
		if n := fake.calls.Load(); n != 1 {
			t.Fatalf("6 concurrent tool calls refreshed %d times, want 1", n)
		}
	})

	t.Run("a dead refresh token reports the connection as needing a reconnect", func(t *testing.T) {
		orgID := testOrg(t, systemPool)
		expiringConnection(t, appPool, encKey, orgID, "fake-refreshable", -time.Minute)
		fake := &racingRefresher{name: "fake-refreshable", err: permanentRefreshErr()}
		_, err := GetFreshIntegrationToken(ctx, appPool, encKey, NewRegistry(fake), orgID, "fake-refreshable")
		if !errors.Is(err, ErrTokenUnavailable) || status(orgID) != "expired" {
			t.Fatalf("got err=%v status=%s, want ErrTokenUnavailable and an expired connection", err, status(orgID))
		}
	})

	t.Run("a temporary failure falls back to the stored token and keeps the connection", func(t *testing.T) {
		orgID := testOrg(t, systemPool)
		expiringConnection(t, appPool, encKey, orgID, "fake-refreshable", time.Minute)
		fake := &racingRefresher{name: "fake-refreshable", err: retrieveErr(502, "")}
		tok, err := GetFreshIntegrationToken(ctx, appPool, encKey, NewRegistry(fake), orgID, "fake-refreshable")
		if err != nil || tok.AccessToken != "stored-access" || status(orgID) != "connected" {
			t.Fatalf("got (%q, %v, %s), want the stored token and a still-connected connection", tok.AccessToken, err, status(orgID))
		}
	})

	t.Run("a service the org never connected is not-connected", func(t *testing.T) {
		orgID := testOrg(t, systemPool)
		_, err := GetFreshIntegrationToken(ctx, appPool, encKey, NewRegistry(), orgID, "fake-refreshable")
		if !errors.Is(err, ErrNotConnected) {
			t.Fatalf("err = %v, want ErrNotConnected", err)
		}
	})
}

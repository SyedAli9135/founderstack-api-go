//go:build integration

package settings

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/core/notify"
)

// viewerInOrgOf adds a viewer to the org adminClerkID belongs to and returns
// the viewer's clerk user id.
func viewerInOrgOf(t *testing.T, systemPool *pgxpool.Pool, adminClerkID string) string {
	t.Helper()
	viewerClerkID := "user_settings_test_viewer_" + response.NewID()[:12]
	_, err := systemPool.Exec(context.Background(),
		`insert into users (org_id, clerk_user_id, email, role)
		 select org_id, $2::text, $2::text || '@example.com', 'viewer' from users where clerk_user_id = $1`,
		adminClerkID, viewerClerkID)
	if err != nil {
		t.Fatalf("insert viewer: %v", err)
	}
	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from users where clerk_user_id = $1", viewerClerkID)
	})
	return viewerClerkID
}

func TestSettings_OrgLevelUpdatesRequireOwnerOrAdmin(t *testing.T) {
	systemPool, appPool, cfg := testSystemPool(t), testAppPool(t), testConfig(t)
	router := testDigestRouter(t, systemPool, appPool, cfg, testEncryptionKey(t), &fakeEmailSender{}, notify.NewDigestTokenSigner("test-digest-secret-access"))
	adminClerkID := testOrgAndUser(t, systemPool)
	viewerClerkID := viewerInOrgOf(t, systemPool, adminClerkID)

	digestBody := map[string]any{"digest_enabled": false, "digest_send_hour": 9, "digest_timezone": "UTC"}
	approvalsBody := map[string]any{"slack_channel_id": "C0123456789"}

	for _, tc := range []struct {
		name, path string
		body       map[string]any
	}{
		{"digest", "/api/v1/settings/digest", digestBody},
		{"approvals", "/api/v1/settings/approvals", approvalsBody},
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, authedRequest(t, cfg, viewerClerkID, http.MethodPut, tc.path, tc.body))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: viewer PUT status = %d, want 403; body = %s", tc.name, rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		router.ServeHTTP(rec, authedRequest(t, cfg, adminClerkID, http.MethodPut, tc.path, tc.body))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: admin PUT status = %d, want 200; body = %s", tc.name, rec.Code, rec.Body.String())
		}
	}

	// A viewer can still read the settings.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, authedRequest(t, cfg, viewerClerkID, http.MethodGet, "/api/v1/settings/digest", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("viewer GET digest status = %d, want 200", rec.Code)
	}
}

func TestSettings_ApprovalsRejectsOversizedChannel(t *testing.T) {
	systemPool, appPool, cfg := testSystemPool(t), testAppPool(t), testConfig(t)
	router := testDigestRouter(t, systemPool, appPool, cfg, testEncryptionKey(t), &fakeEmailSender{}, notify.NewDigestTokenSigner("test-digest-secret-access"))
	adminClerkID := testOrgAndUser(t, systemPool)

	body := map[string]any{"slack_channel_id": strings.Repeat("C", 65)}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, authedRequest(t, cfg, adminClerkID, http.MethodPut, "/api/v1/settings/approvals", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestSettings_PushSubscriptionRejectsNonPushEndpoints(t *testing.T) {
	systemPool, appPool, cfg := testSystemPool(t), testAppPool(t), testConfig(t)
	router := testDigestRouter(t, systemPool, appPool, cfg, testEncryptionKey(t), &fakeEmailSender{}, notify.NewDigestTokenSigner("test-digest-secret-access"))
	clerkID := testOrgAndUser(t, systemPool)

	post := func(endpoint string) int {
		body := map[string]any{"endpoint": endpoint, "keys": map[string]string{"p256dh": "k", "auth": "a"}}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, authedRequest(t, cfg, clerkID, http.MethodPost, "/api/v1/settings/push-subscription", body))
		if rec.Code == http.StatusBadRequest && !strings.Contains(rec.Body.String(), "INVALID_PUSH_ENDPOINT") {
			t.Errorf("endpoint %q: 400 without INVALID_PUSH_ENDPOINT: %s", endpoint, rec.Body.String())
		}
		return rec.Code
	}

	for _, bad := range []string{"https://169.254.169.254/latest/meta-data", "http://fcm.googleapis.com/x", "https://localhost/x"} {
		if got := post(bad); got != http.StatusBadRequest {
			t.Errorf("endpoint %q: status = %d, want 400", bad, got)
		}
	}
	if got := post("https://fcm.googleapis.com/fcm/send/abc123"); got != http.StatusOK {
		t.Errorf("real FCM endpoint: status = %d, want 200", got)
	}
}

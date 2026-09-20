//go:build integration

package settings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/core/notify"
)

// fakeEmailSender is the one EmailSender test double in this codebase --
// nothing needed one before workflow 20, since approval-gate emails were
// only ever exercised as a logged no-op (BREVO_API_KEY unset in every
// other integration test's config).
type fakeEmailSender struct {
	mu   sync.Mutex
	sent []sentEmail
}

type sentEmail struct{ to, subject, textBody, htmlBody string }

func (f *fakeEmailSender) Send(ctx context.Context, toEmail, subject, textBody, htmlBody string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentEmail{to: toEmail, subject: subject, textBody: textBody, htmlBody: htmlBody})
	return nil
}

func (f *fakeEmailSender) last() sentEmail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[len(f.sent)-1]
}

func (f *fakeEmailSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func testDigestRouter(t *testing.T, systemPool, appPool *pgxpool.Pool, cfg *config.Config, encKey []byte, email notify.EmailSender, tokens *notify.DigestTokenSigner) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())

	h := NewHandler(appPool, encKey, cfg.APIKeyMockPrefix, email, tokens, "http://localhost:8000")

	authed := r.Group("/api/v1/settings")
	authed.Use(middleware.RequireAuth(systemPool, cfg))
	h.Register(authed)

	// Mirrors main.go's real wiring: same path prefix, no RequireAuth --
	// Unsubscribe does its own token-based auth.
	public := r.Group("/api/v1/settings")
	h.RegisterPublic(public)

	return r
}

func TestSettingsDigest_SettingsLifecycle(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	cfg := testConfig(t)
	encKey := testEncryptionKey(t)
	tokens := notify.NewDigestTokenSigner("test-digest-secret-lifecycle")
	router := testDigestRouter(t, systemPool, appPool, cfg, encKey, &fakeEmailSender{}, tokens)
	clerkUserID := testOrgAndUser(t, systemPool)

	type envelope struct {
		Data digestSettings `json:"data"`
	}

	t.Run("defaults are sane before any update", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/settings/digest", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		var got envelope
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if !got.Data.DigestEnabled || got.Data.DigestSendHour != 8 || got.Data.DigestTimezone != "UTC" {
			t.Fatalf("defaults = %+v, want enabled/8/UTC", got.Data)
		}
	})

	t.Run("rejects an unrecognized IANA timezone", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPut, "/api/v1/settings/digest",
			map[string]any{"digest_enabled": true, "digest_send_hour": 9, "digest_timezone": "Mars/Phobos"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("rejects an out-of-range send hour", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPut, "/api/v1/settings/digest",
			map[string]any{"digest_enabled": true, "digest_send_hour": 24, "digest_timezone": "UTC"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a valid update round-trips through a subsequent GET", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPut, "/api/v1/settings/digest",
			map[string]any{"digest_enabled": false, "digest_send_hour": 14, "digest_timezone": "America/New_York"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}

		req2 := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/settings/digest", nil)
		rec2 := httptest.NewRecorder()
		router.ServeHTTP(rec2, req2)
		var got envelope
		if err := json.Unmarshal(rec2.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Data.DigestEnabled || got.Data.DigestSendHour != 14 || got.Data.DigestTimezone != "America/New_York" {
			t.Fatalf("after update = %+v, want disabled/14/America/New_York", got.Data)
		}
	})
}

func TestSettingsDigest_TestSendAndUnsubscribe(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	cfg := testConfig(t)
	encKey := testEncryptionKey(t)
	email := &fakeEmailSender{}
	tokens := notify.NewDigestTokenSigner("test-digest-secret-send")
	router := testDigestRouter(t, systemPool, appPool, cfg, encKey, email, tokens)
	clerkUserID := testOrgAndUser(t, systemPool)

	var orgID pgtype.UUID
	if err := systemPool.QueryRow(context.Background(),
		"select org_id from users where clerk_user_id = $1", clerkUserID,
	).Scan(&orgID); err != nil {
		t.Fatalf("look up test org id: %v", err)
	}

	t.Run("delivers a real digest to the caller's own email, not every org member", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/settings/digest/test", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		if email.count() != 1 {
			t.Fatalf("emails sent = %d, want 1", email.count())
		}
		got := email.last()
		if got.to != "settings-test@example.com" {
			t.Errorf("sent to %q, want the caller's own email", got.to)
		}
		// No runs were ever seeded for this org, so BuildPayload must
		// report HadActivity=false and RenderEmail must take that branch.
		if !strings.Contains(got.textBody, "No runs yesterday") {
			t.Errorf("text body = %q, missing the no-activity message", got.textBody)
		}
		if !strings.Contains(got.htmlBody, "<html") {
			t.Error("html body doesn't look like rendered HTML")
		}
	})

	t.Run("unsubscribe link disables the digest with no auth header", func(t *testing.T) {
		token := tokens.Sign(uuid.UUID(orgID.Bytes))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/digest/unsubscribe?token="+token, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}

		var enabled bool
		if err := systemPool.QueryRow(context.Background(),
			"select digest_enabled from organizations where id = $1", orgID,
		).Scan(&enabled); err != nil {
			t.Fatal(err)
		}
		if enabled {
			t.Error("digest_enabled still true after hitting the unsubscribe link")
		}
	})

	t.Run("a garbage token is rejected, not silently accepted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/digest/unsubscribe?token=not-a-real-token", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a token signed by a different secret is rejected", func(t *testing.T) {
		otherTokens := notify.NewDigestTokenSigner("a-completely-different-secret")
		token := otherTokens.Sign(uuid.UUID(orgID.Bytes))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/digest/unsubscribe?token="+token, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})
}

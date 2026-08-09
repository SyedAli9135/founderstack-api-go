//go:build integration

// Integration tests exercise the real HTTP handler against a real
// Postgres, connected as app_system — the same role production uses.
// Excluded from a plain `go test ./...` by the build tag (no DB needed for
// the default run); included via `go test -tags=integration ./...`, which
// is what CI runs. Set TEST_SYSTEM_DATABASE_URL to opt in locally too
// (`make test-integration` does this against the local docker Postgres).
//
// These automate exactly the 8 scenarios that were originally verified by
// hand with curl+psql during development — the point is that a future
// change breaking idempotency, the org-not-found-yet retry path, or the
// soft-delete behavior now fails a test instead of requiring another
// manual session to notice.
package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/api/response"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_SYSTEM_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_SYSTEM_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testSecret(t *testing.T) (string, []byte) {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return "whsec_" + base64.StdEncoding.EncodeToString(raw), raw
}

func testRouter(t *testing.T, pool *pgxpool.Pool, secret string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())
	NewClerkHandler(pool, secret).Register(r.Group("/api/webhooks"))
	return r
}

// sign reimplements the signing side directly (mirrors verify_test.go's
// helper) rather than depending on the production code under test.
func sign(t *testing.T, secretBytes []byte, id, timestamp string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, secretBytes)
	mac.Write([]byte(id + "." + timestamp + "." + string(body)))
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func postWebhook(t *testing.T, router *gin.Engine, secretBytes []byte, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	id := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/clerk", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("svix-id", id)
	req.Header.Set("svix-timestamp", timestamp)
	req.Header.Set("svix-signature", sign(t, secretBytes, id, timestamp, body))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestClerkWebhook_FullLifecycle(t *testing.T) {
	pool := testPool(t)
	secret, secretBytes := testSecret(t)
	router := testRouter(t, pool, secret)

	suffix := response.NewID()[:12]
	orgClerkID := "org_test_" + suffix
	orgSlug := "test-org-" + suffix
	userClerkID := "user_test_" + suffix
	userClerkID2 := "user_test2_" + suffix

	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "delete from users where clerk_user_id in ($1, $2, $3)",
			userClerkID, userClerkID2, "user_never_synced_"+suffix)
		_, _ = pool.Exec(ctx, "delete from organizations where clerk_org_id = $1", orgClerkID)
	})

	t.Run("membership before its org exists returns 422", func(t *testing.T) {
		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": "organizationMembership.created",
			"data": map[string]any{
				"organization": map[string]any{"id": orgClerkID, "name": "Test Org", "slug": orgSlug},
				"public_user_data": map[string]any{
					"user_id": userClerkID, "identifier": "founder@example.com",
					"first_name": "Ada", "last_name": "Lovelace",
				},
				"role": "org:admin",
			},
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("organization.created creates the org", func(t *testing.T) {
		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": "organization.created",
			"data": map[string]any{"id": orgClerkID, "name": "Test Org", "slug": orgSlug},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}

		var name, slug string
		var isActive bool
		err := pool.QueryRow(context.Background(),
			"select name, slug, is_active from organizations where clerk_org_id = $1", orgClerkID,
		).Scan(&name, &slug, &isActive)
		if err != nil {
			t.Fatalf("query organization: %v", err)
		}
		if name != "Test Org" || slug != orgSlug || !isActive {
			t.Fatalf("organization row = (%q, %q, %v), want (\"Test Org\", %q, true)", name, slug, isActive, orgSlug)
		}
	})

	t.Run("replaying organization.created is idempotent", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			rec := postWebhook(t, router, secretBytes, map[string]any{
				"type": "organization.created",
				"data": map[string]any{"id": orgClerkID, "name": "Test Org", "slug": orgSlug},
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("replay %d: status = %d, want 200", i, rec.Code)
			}
		}
		var count int
		if err := pool.QueryRow(context.Background(),
			"select count(*) from organizations where clerk_org_id = $1", orgClerkID,
		).Scan(&count); err != nil {
			t.Fatalf("count organizations: %v", err)
		}
		if count != 1 {
			t.Fatalf("row count = %d, want 1 (idempotency violated)", count)
		}
	})

	t.Run("organizationMembership.created creates the user now that the org exists", func(t *testing.T) {
		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": "organizationMembership.created",
			"data": map[string]any{
				"organization": map[string]any{"id": orgClerkID, "name": "Test Org", "slug": orgSlug},
				"public_user_data": map[string]any{
					"user_id": userClerkID, "identifier": "founder@example.com",
					"first_name": "Ada", "last_name": "Lovelace",
				},
				"role": "org:admin",
			},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}

		var email, fullName, role string
		err := pool.QueryRow(context.Background(),
			"select email, full_name, role from users where clerk_user_id = $1", userClerkID,
		).Scan(&email, &fullName, &role)
		if err != nil {
			t.Fatalf("query user: %v", err)
		}
		if email != "founder@example.com" || fullName != "Ada Lovelace" || role != "admin" {
			t.Fatalf("user row = (%q, %q, %q), want (\"founder@example.com\", \"Ada Lovelace\", \"admin\")", email, fullName, role)
		}
	})

	t.Run("organization.updated updates the org row", func(t *testing.T) {
		// Distinct from "organization.created creates the org" above: that
		// test never actually sends the literal string "organization.updated"
		// through Handle's switch — both share upsertOrganization, but a typo
		// in the case label (e.g. "organisation.updated") would silently fall
		// through to the default/unhandled branch and this is the only test
		// that would catch it.
		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": "organization.updated",
			"data": map[string]any{"id": orgClerkID, "name": "Renamed Org", "slug": orgSlug},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		var name string
		if err := pool.QueryRow(context.Background(),
			"select name from organizations where clerk_org_id = $1", orgClerkID,
		).Scan(&name); err != nil {
			t.Fatalf("query organization: %v", err)
		}
		if name != "Renamed Org" {
			t.Fatalf("name = %q, want %q", name, "Renamed Org")
		}
	})

	t.Run("user.updated updates name and avatar", func(t *testing.T) {
		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": "user.updated",
			"data": map[string]any{
				"id": userClerkID, "first_name": "Ada", "last_name": "Byron",
				"image_url": "https://img.clerk.com/ada.png",
			},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}

		var fullName, avatarURL string
		if err := pool.QueryRow(context.Background(),
			"select full_name, avatar_url from users where clerk_user_id = $1", userClerkID,
		).Scan(&fullName, &avatarURL); err != nil {
			t.Fatalf("query user: %v", err)
		}
		if fullName != "Ada Byron" || avatarURL != "https://img.clerk.com/ada.png" {
			t.Fatalf("user row = (%q, %q), want (\"Ada Byron\", \"https://img.clerk.com/ada.png\")", fullName, avatarURL)
		}
	})

	t.Run("user.updated for a never-synced user is a silent no-op, not an error", func(t *testing.T) {
		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": "user.updated",
			"data": map[string]any{"id": "user_never_synced_" + suffix, "first_name": "Ghost", "last_name": "User"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("organizationMembership.updated changes the role", func(t *testing.T) {
		// Same rationale as "organization.updated" above: exercises the
		// literal "organizationMembership.updated" event-type string, not
		// just the shared upsertMembership function via .created.
		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": "organizationMembership.updated",
			"data": map[string]any{
				"organization": map[string]any{"id": orgClerkID, "name": "Renamed Org", "slug": orgSlug},
				"public_user_data": map[string]any{
					"user_id": userClerkID, "identifier": "founder@example.com",
					"first_name": "Ada", "last_name": "Lovelace",
				},
				"role": "org:member",
			},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		var role string
		if err := pool.QueryRow(context.Background(),
			"select role from users where clerk_user_id = $1", userClerkID,
		).Scan(&role); err != nil {
			t.Fatalf("query user: %v", err)
		}
		if role != "member" {
			t.Fatalf("role = %q, want %q", role, "member")
		}
	})

	t.Run("organizationMembership.deleted soft-deletes the user", func(t *testing.T) {
		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": "organizationMembership.deleted",
			"data": map[string]any{
				"organization": map[string]any{"id": orgClerkID, "name": "Test Org", "slug": orgSlug},
				"public_user_data": map[string]any{
					"user_id": userClerkID, "identifier": "founder@example.com",
					"first_name": "Ada", "last_name": "Byron",
				},
				"role": "org:admin",
			},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}

		var isActive bool
		if err := pool.QueryRow(context.Background(),
			"select is_active from users where clerk_user_id = $1", userClerkID,
		).Scan(&isActive); err != nil {
			t.Fatalf("user row missing after membership removal (should survive): %v", err)
		}
		if isActive {
			t.Fatal("is_active = true, want false after organizationMembership.deleted")
		}
	})

	t.Run("user.deleted soft-deletes the user (account deleted entirely)", func(t *testing.T) {
		// Independent fixture: create a second membership fresh so this test
		// isn't relying on the ordering/state of the membership.deleted test
		// above — it asserts a true->false transition on its own user.
		setup := postWebhook(t, router, secretBytes, map[string]any{
			"type": "organizationMembership.created",
			"data": map[string]any{
				"organization": map[string]any{"id": orgClerkID, "name": "Test Org", "slug": orgSlug},
				"public_user_data": map[string]any{
					"user_id": userClerkID2, "identifier": "cofounder@example.com",
					"first_name": "Grace", "last_name": "Hopper",
				},
				"role": "org:member",
			},
		})
		if setup.Code != http.StatusOK {
			t.Fatalf("fixture setup: status = %d, want 200; body = %s", setup.Code, setup.Body.String())
		}

		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": "user.deleted",
			"data": map[string]any{"id": userClerkID2},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}

		var isActive bool
		if err := pool.QueryRow(context.Background(),
			"select is_active from users where clerk_user_id = $1", userClerkID2,
		).Scan(&isActive); err != nil {
			t.Fatalf("user row missing after user.deleted (should survive): %v", err)
		}
		if isActive {
			t.Fatal("is_active = true, want false after user.deleted")
		}
	})

	t.Run("organization.deleted soft-deletes: row survives, is_active flips false", func(t *testing.T) {
		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": "organization.deleted",
			"data": map[string]any{"id": orgClerkID},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}

		var isActive bool
		err := pool.QueryRow(context.Background(),
			"select is_active from organizations where clerk_org_id = $1", orgClerkID,
		).Scan(&isActive)
		if err != nil {
			t.Fatalf("organization row missing after soft-delete (should survive): %v", err)
		}
		if isActive {
			t.Fatal("is_active = true, want false after organization.deleted")
		}
	})
}

func TestClerkWebhook_RejectsForgedSignature(t *testing.T) {
	pool := testPool(t)
	secret, _ := testSecret(t)
	router := testRouter(t, pool, secret)

	orgClerkID := "org_evil_" + response.NewID()[:12]
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "delete from organizations where clerk_org_id = $1", orgClerkID)
	})

	body, _ := json.Marshal(map[string]any{
		"type": "organization.created",
		"data": map[string]any{"id": orgClerkID, "name": "Evil Org", "slug": "evil-org"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/clerk", bytes.NewReader(body))
	req.Header.Set("svix-id", "msg_forged")
	req.Header.Set("svix-timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set("svix-signature", "v1,bm90LWEtcmVhbC1zaWduYXR1cmU=")

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		"select count(*) from organizations where clerk_org_id = $1", orgClerkID,
	).Scan(&count); err != nil {
		t.Fatalf("count organizations: %v", err)
	}
	if count != 0 {
		t.Fatal("forged webhook was persisted despite an invalid signature")
	}
}

func TestClerkWebhook_RejectsMissingSvixHeaders(t *testing.T) {
	pool := testPool(t)
	secret, _ := testSecret(t)
	router := testRouter(t, pool, secret)

	body, _ := json.Marshal(map[string]any{
		"type": "organization.created",
		"data": map[string]any{"id": "org_no_headers", "name": "X", "slug": "x"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/clerk", bytes.NewReader(body))
	// Deliberately no svix-id/svix-timestamp/svix-signature headers at all —
	// distinct from TestClerkWebhook_RejectsForgedSignature, which sends
	// headers with a wrong signature. This exercises MISSING_SVIX_HEADERS,
	// not INVALID_SIGNATURE — a different branch in Handle.

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("MISSING_SVIX_HEADERS")) {
		t.Fatalf("body = %s, want it to contain MISSING_SVIX_HEADERS", rec.Body.String())
	}
}

func TestClerkWebhook_RejectsMalformedJSONBody(t *testing.T) {
	pool := testPool(t)
	secret, secretBytes := testSecret(t)
	router := testRouter(t, pool, secret)

	body := []byte(`this is not json`)
	id := "msg_malformed"
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/clerk", bytes.NewReader(body))
	req.Header.Set("svix-id", id)
	req.Header.Set("svix-timestamp", timestamp)
	req.Header.Set("svix-signature", sign(t, secretBytes, id, timestamp, body))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("INVALID_PAYLOAD")) {
		t.Fatalf("body = %s, want it to contain INVALID_PAYLOAD", rec.Body.String())
	}
}

func TestClerkWebhook_UnknownEventTypeIsAckedWithoutWriting(t *testing.T) {
	pool := testPool(t)
	secret, secretBytes := testSecret(t)
	router := testRouter(t, pool, secret)

	orgClerkID := "org_from_session_event_" + response.NewID()[:12]
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "delete from organizations where clerk_org_id = $1", orgClerkID)
	})

	// session.created is real Clerk event this backend deliberately doesn't
	// act on (see the `default` case in Handle) — reusing an org id in the
	// data payload here only to prove nothing gets written from it.
	rec := postWebhook(t, router, secretBytes, map[string]any{
		"type": "session.created",
		"data": map[string]any{"id": orgClerkID},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unhandled types are acked, not rejected); body = %s", rec.Code, rec.Body.String())
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		"select count(*) from organizations where clerk_org_id = $1", orgClerkID,
	).Scan(&count); err != nil {
		t.Fatalf("count organizations: %v", err)
	}
	if count != 0 {
		t.Fatal("an unhandled event type wrote to the database — it should be a pure no-op")
	}
}

func TestClerkWebhook_MalformedEventDataReturns500(t *testing.T) {
	pool := testPool(t)
	secret, secretBytes := testSecret(t)
	router := testRouter(t, pool, secret)

	// A known, handled event type, but "data" is a string instead of an
	// object — organizationPayload's json.Unmarshal fails, exercising the
	// generic WEBHOOK_PROCESSING_FAILED / 500 path rather than any of the
	// specific 4xx branches above it in Handle.
	rec := postWebhook(t, router, secretBytes, map[string]any{
		"type": "organization.created",
		"data": "this should be an object, not a string",
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("WEBHOOK_PROCESSING_FAILED")) {
		t.Fatalf("body = %s, want it to contain WEBHOOK_PROCESSING_FAILED", rec.Body.String())
	}
}

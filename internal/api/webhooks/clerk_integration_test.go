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
	"strings"
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
		var canApprove, canManageKeys, canManageIntegrations bool
		if err := pool.QueryRow(context.Background(),
			"select role, can_approve_workflows, can_manage_api_keys, can_manage_integrations from users where clerk_user_id = $1", userClerkID,
		).Scan(&role, &canApprove, &canManageKeys, &canManageIntegrations); err != nil {
			t.Fatalf("query user: %v", err)
		}
		if role != "member" {
			t.Fatalf("role = %q, want %q", role, "member")
		}
		// Clerk really changed this person's role (org:admin -> org:member), so
		// the permission flags follow it. Keeping the admin-era flags would
		// leave a demoted member able to manage keys, integrations and approvals.
		if canApprove || canManageKeys || canManageIntegrations {
			t.Fatalf("flags = (%v, %v, %v), want all false — a role demoted in Clerk must take the flags with it",
				canApprove, canManageKeys, canManageIntegrations)
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

	t.Run("organizationMembership.created after removal resets stale permission flags", func(t *testing.T) {
		// Regression test for a real bug found live 2026-09-07: a removed
		// member's row keeps its old admin-era flags. Recreate that stale state
		// explicitly (role changes in Clerk now recompute flags, so nothing
		// earlier in this test leaves them true).
		if _, err := pool.Exec(context.Background(),
			"update users set can_approve_workflows = true, can_manage_api_keys = true, can_manage_integrations = true where clerk_user_id = $1", userClerkID); err != nil {
			t.Fatal(err)
		}
		// A brand new membership (this user was removed, then re-invited as a
		// plain member) must reset those stale admin-era flags to the new
		// role's default: the prior membership is over.
		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": "organizationMembership.created",
			"data": map[string]any{
				"organization": map[string]any{"id": orgClerkID, "name": "Test Org", "slug": orgSlug},
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

		var isActive, canApprove, canManageKeys, canManageIntegrations bool
		if err := pool.QueryRow(context.Background(),
			"select is_active, can_approve_workflows, can_manage_api_keys, can_manage_integrations from users where clerk_user_id = $1", userClerkID,
		).Scan(&isActive, &canApprove, &canManageKeys, &canManageIntegrations); err != nil {
			t.Fatalf("query user: %v", err)
		}
		if !isActive {
			t.Fatal("is_active = false, want true after re-added via organizationMembership.created")
		}
		if canApprove || canManageKeys || canManageIntegrations {
			t.Fatalf("flags = (%v, %v, %v), want all false — a reactivated membership must reset stale flags from a prior, terminated membership",
				canApprove, canManageKeys, canManageIntegrations)
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

// Workflow 21: one Clerk user can belong to several orgs (a practice plus
// its client workspaces), each its own users row.
func TestClerkWebhook_MultiOrgMembership(t *testing.T) {
	pool := testPool(t)
	secret, secretBytes := testSecret(t)
	router := testRouter(t, pool, secret)
	ctx := context.Background()

	suffix := response.NewID()[:12]
	orgA, orgB := "org_multi_a_"+suffix, "org_multi_b_"+suffix
	userID := "user_multi_" + suffix
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "delete from users where clerk_user_id = $1", userID)
		_, _ = pool.Exec(context.Background(), "delete from organizations where clerk_org_id in ($1, $2)", orgA, orgB)
	})

	membership := func(eventType, clerkOrgID string) *httptest.ResponseRecorder {
		return postWebhook(t, router, secretBytes, map[string]any{
			"type": eventType,
			"data": map[string]any{
				"organization": map[string]any{"id": clerkOrgID},
				"public_user_data": map[string]any{
					"user_id": userID, "identifier": "operator@example.com", "first_name": "Grace", "last_name": "Hopper",
				},
				"role": "org:admin",
			},
		})
	}
	activeRows := func() map[string]bool {
		rows, err := pool.Query(ctx,
			`select o.clerk_org_id, u.is_active from users u join organizations o on o.id = u.org_id
			 where u.clerk_user_id = $1`, userID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var org string
			var active bool
			if err := rows.Scan(&org, &active); err != nil {
				t.Fatal(err)
			}
			out[org] = active
		}
		return out
	}

	for _, org := range []string{orgA, orgB} {
		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": "organization.created", "data": map[string]any{"id": org, "name": "Multi " + org, "slug": "multi-" + org[len(org)-14:]},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("organization.created %s: status = %d; body = %s", org, rec.Code, rec.Body.String())
		}
	}

	t.Run("joining two orgs creates two rows, not one overwritten row", func(t *testing.T) {
		for _, org := range []string{orgA, orgB} {
			if rec := membership("organizationMembership.created", org); rec.Code != http.StatusOK {
				t.Fatalf("membership %s: status = %d; body = %s", org, rec.Code, rec.Body.String())
			}
		}
		got := activeRows()
		if len(got) != 2 || !got[orgA] || !got[orgB] {
			t.Fatalf("memberships = %v, want active rows in both %s and %s", got, orgA, orgB)
		}
	})

	t.Run("replaying a membership event stays idempotent per org", func(t *testing.T) {
		if rec := membership("organizationMembership.updated", orgA); rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := activeRows(); len(got) != 2 {
			t.Fatalf("memberships = %v, want still exactly 2 rows", got)
		}
	})

	t.Run("user.updated updates the profile on every membership row", func(t *testing.T) {
		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": "user.updated",
			"data": map[string]any{"id": userID, "first_name": "Rear Admiral", "last_name": "Hopper", "image_url": "https://img.example.com/g.png"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		var n int
		if err := pool.QueryRow(ctx,
			`select count(*) from users where clerk_user_id = $1 and full_name = 'Rear Admiral Hopper' and avatar_url = 'https://img.example.com/g.png'`,
			userID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Fatalf("updated rows = %d, want 2", n)
		}
	})

	t.Run("membership.deleted in one org leaves the other membership untouched", func(t *testing.T) {
		if rec := membership("organizationMembership.deleted", orgA); rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		got := activeRows()
		if got[orgA] || !got[orgB] {
			t.Fatalf("memberships = %v, want %s inactive and %s still active", got, orgA, orgB)
		}
	})

	t.Run("organization.updated never resurrects a deactivated org", func(t *testing.T) {
		if _, err := pool.Exec(ctx, "update organizations set is_active = false where clerk_org_id = $1", orgB); err != nil {
			t.Fatal(err)
		}
		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": "organization.updated", "data": map[string]any{"id": orgB, "name": "Renamed", "slug": "multi-" + orgB[len(orgB)-14:]},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		var name string
		var active bool
		if err := pool.QueryRow(ctx, "select name, is_active from organizations where clerk_org_id = $1", orgB).Scan(&name, &active); err != nil {
			t.Fatal(err)
		}
		if name != "Renamed" || active {
			t.Fatalf("org = (%q, active=%v), want renamed but still inactive", name, active)
		}
	})

	t.Run("user.deleted deactivates every membership", func(t *testing.T) {
		membership("organizationMembership.created", orgA)
		rec := postWebhook(t, router, secretBytes, map[string]any{"type": "user.deleted", "data": map[string]any{"id": userID}})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		for org, active := range activeRows() {
			if active {
				t.Fatalf("membership in %s still active after user.deleted", org)
			}
		}
	})
}

// Clerk instances with org slugs disabled send no slug at all.
func TestClerkWebhook_OrgsWithoutSlugs(t *testing.T) {
	pool := testPool(t)
	secret, secretBytes := testSecret(t)
	router := testRouter(t, pool, secret)
	ctx := context.Background()

	suffix := response.NewID()[:12]
	orgA, orgB := "org_noslug_a_"+suffix, "org_noslug_b_"+suffix
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "delete from organizations where clerk_org_id in ($1, $2)", orgA, orgB)
	})

	for _, org := range []string{orgA, orgB} {
		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": "organization.created", "data": map[string]any{"id": org, "name": "No Slug", "slug": nil},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("organization.created %s without slug: status = %d; body = %s", org, rec.Code, rec.Body.String())
		}
	}

	t.Run("a slugless update never clobbers a stored slug", func(t *testing.T) {
		if _, err := pool.Exec(ctx, "update organizations set slug = $2 where clerk_org_id = $1", orgA, "real-slug-"+suffix); err != nil {
			t.Fatal(err)
		}
		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": "organization.updated", "data": map[string]any{"id": orgA, "name": "Renamed", "slug": nil},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		var slug, name string
		if err := pool.QueryRow(ctx, "select slug, name from organizations where clerk_org_id = $1", orgA).Scan(&slug, &name); err != nil {
			t.Fatal(err)
		}
		if slug != "real-slug-"+suffix || name != "Renamed" {
			t.Fatalf("org = (%q, %q), want the stored slug kept and the name updated", slug, name)
		}
	})
}

// Clerk doesn't guarantee order and retries failed deliveries, so an old event
// can arrive after a newer one. It must not re-activate a removed member,
// undo a removal that came after it, or revert a role.
func TestClerkWebhook_StaleEventsCannotUndoNewerOnes(t *testing.T) {
	pool := testPool(t)
	secret, secretBytes := testSecret(t)
	router := testRouter(t, pool, secret)

	suffix := response.NewID()[:12]
	orgClerkID := "org_stale_" + suffix
	userClerkID := "user_stale_" + suffix
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "delete from users where clerk_user_id = $1", userClerkID)
		_, _ = pool.Exec(ctx, "delete from organizations where clerk_org_id = $1", orgClerkID)
	})
	if rec := postWebhook(t, router, secretBytes, map[string]any{
		"type": "organization.created",
		"data": map[string]any{"id": orgClerkID, "name": "Stale Org", "slug": "stale-" + suffix},
	}); rec.Code != http.StatusOK {
		t.Fatalf("org create = %d", rec.Code)
	}

	base := time.Now().UnixMilli()
	membership := func(typ, role string, offsetMs int64) *httptest.ResponseRecorder {
		return postWebhook(t, router, secretBytes, map[string]any{
			"type": typ, "timestamp": base + offsetMs,
			"data": map[string]any{
				"organization":     map[string]any{"id": orgClerkID, "name": "Stale Org", "slug": "stale-" + suffix},
				"public_user_data": map[string]any{"user_id": userClerkID, "identifier": "stale@example.com"},
				"role":             role,
			},
		})
	}
	state := func() (active bool, role string) {
		if err := pool.QueryRow(context.Background(), "select is_active, role from users where clerk_user_id = $1", userClerkID).Scan(&active, &role); err != nil {
			t.Fatalf("query user: %v", err)
		}
		return
	}

	if rec := membership("organizationMembership.created", "org:member", 1000); rec.Code != http.StatusOK {
		t.Fatalf("created = %d", rec.Code)
	}
	if rec := membership("organizationMembership.deleted", "org:member", 3000); rec.Code != http.StatusOK {
		t.Fatalf("deleted = %d", rec.Code)
	}

	t.Run("a stale created event can't re-activate a removed member", func(t *testing.T) {
		if rec := membership("organizationMembership.created", "org:admin", 2000); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (ack it, change nothing)", rec.Code)
		}
		if active, role := state(); active || role != "member" {
			t.Fatalf("after a stale created: active=%v role=%q, want inactive member", active, role)
		}
	})

	t.Run("a genuinely newer created event (a re-invite) is applied", func(t *testing.T) {
		membership("organizationMembership.created", "org:member", 4000)
		if active, _ := state(); !active {
			t.Fatal("a newer created event should re-activate the member")
		}
	})

	t.Run("a stale deleted event can't remove a newer membership", func(t *testing.T) {
		membership("organizationMembership.deleted", "org:member", 3500)
		if active, _ := state(); !active {
			t.Fatal("a deleted event older than the re-invite must not deactivate it")
		}
	})

	t.Run("a stale updated event can't revert a role", func(t *testing.T) {
		membership("organizationMembership.updated", "org:admin", 3600)
		if _, role := state(); role != "member" {
			t.Fatalf("role = %q after a stale updated, want member", role)
		}
		membership("organizationMembership.updated", "org:admin", 5000)
		if _, role := state(); role != "admin" {
			t.Fatalf("role = %q after a newer updated, want admin", role)
		}
	})

	t.Run("a stale user.deleted can't deactivate a newer membership, a fresh one can", func(t *testing.T) {
		userDeleted := func(offsetMs int64) {
			postWebhook(t, router, secretBytes, map[string]any{
				"type": "user.deleted", "timestamp": base + offsetMs, "data": map[string]any{"id": userClerkID},
			})
		}
		userDeleted(100)
		if active, _ := state(); !active {
			t.Fatal("a user.deleted older than the membership must not deactivate it")
		}
		userDeleted(9000)
		if active, _ := state(); active {
			t.Fatal("a newer user.deleted should deactivate")
		}
	})
}

func TestClerkWebhook_RejectsAnOversizedBodyBeforeDoingAnyWork(t *testing.T) {
	pool := testPool(t)
	secret, secretBytes := testSecret(t)
	router := testRouter(t, pool, secret)

	body := bytes.Repeat([]byte("a"), maxClerkPayload+1024)
	id, ts := "msg_big", strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/clerk", bytes.NewReader(body))
	req.Header.Set("svix-id", id)
	req.Header.Set("svix-timestamp", ts)
	req.Header.Set("svix-signature", sign(t, secretBytes, id, ts, body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "INVALID_BODY") {
		t.Fatalf("got (%d, %s), want 400 INVALID_BODY for a body over %d bytes", rec.Code, rec.Body.String(), maxClerkPayload)
	}
}

func TestClerkWebhook_MembershipEventsWithoutIDsAreAckedAndWriteNothing(t *testing.T) {
	pool := testPool(t)
	secret, secretBytes := testSecret(t)
	router := testRouter(t, pool, secret)

	var before int
	if err := pool.QueryRow(context.Background(), "select count(*) from users where clerk_user_id = ''").Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"organizationMembership.created", "organizationMembership.updated", "organizationMembership.deleted"} {
		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": typ,
			"data": map[string]any{
				"organization":     map[string]any{"id": "org_whatever", "name": "x", "slug": "x"},
				"public_user_data": map[string]any{"user_id": "", "identifier": "nobody@example.invalid"},
				"role":             "org:admin",
			},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s with no user id = %d, want 200 (ack, nothing to retry)", typ, rec.Code)
		}
	}
	var after int
	if err := pool.QueryRow(context.Background(), "select count(*) from users where clerk_user_id = ''").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("rows with an empty clerk_user_id went from %d to %d", before, after)
	}
}

// users.role is what the app enforces and can be finer-grained than Clerk's
// (a viewer; a demotion that didn't reach Clerk). A membership event that
// carries the role Clerk already had must not undo it, while a role that
// genuinely changed in Clerk must apply — flags included.
func TestClerkWebhook_RoleOnlyFollowsGenuineClerkChanges(t *testing.T) {
	pool := testPool(t)
	secret, secretBytes := testSecret(t)
	router := testRouter(t, pool, secret)

	suffix := response.NewID()[:12]
	orgClerkID, userClerkID := "org_role_"+suffix, "user_role_"+suffix
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "delete from users where clerk_user_id = $1", userClerkID)
		_, _ = pool.Exec(ctx, "delete from organizations where clerk_org_id = $1", orgClerkID)
	})
	if rec := postWebhook(t, router, secretBytes, map[string]any{
		"type": "organization.created",
		"data": map[string]any{"id": orgClerkID, "name": "Role Org", "slug": "role-" + suffix},
	}); rec.Code != http.StatusOK {
		t.Fatalf("org create = %d", rec.Code)
	}
	membership := func(typ, role string) {
		t.Helper()
		rec := postWebhook(t, router, secretBytes, map[string]any{
			"type": typ,
			"data": map[string]any{
				"organization":     map[string]any{"id": orgClerkID, "name": "Role Org", "slug": "role-" + suffix},
				"public_user_data": map[string]any{"user_id": userClerkID, "identifier": "role@example.invalid"},
				"role":             role,
			},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s = %d", typ, role, rec.Code)
		}
	}
	state := func() (role string, canManageKeys bool) {
		t.Helper()
		if err := pool.QueryRow(context.Background(), "select role, can_manage_api_keys from users where clerk_user_id = $1", userClerkID).Scan(&role, &canManageKeys); err != nil {
			t.Fatal(err)
		}
		return
	}
	setLocal := func(role string, flag bool) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), "update users set role = $1, can_manage_api_keys = $2 where clerk_user_id = $3", role, flag, userClerkID); err != nil {
			t.Fatal(err)
		}
	}

	membership("organizationMembership.created", "org:member")

	t.Run("an unrelated event doesn't undo an app-side role (a viewer stays a viewer)", func(t *testing.T) {
		setLocal("viewer", false)
		membership("organizationMembership.updated", "org:member")
		if role, _ := state(); role != "viewer" {
			t.Fatalf("role = %q after an unrelated event, want viewer", role)
		}
	})

	t.Run("an admin demoted in the app isn't re-promoted by a later event", func(t *testing.T) {
		membership("organizationMembership.updated", "org:admin") // Clerk really changed: now admin
		if role, keys := state(); role != "admin" || !keys {
			t.Fatalf("after a real Clerk promotion: role=%q keys=%v, want admin/true", role, keys)
		}
		setLocal("member", false) // the app demotes them (Clerk never got the change)
		membership("organizationMembership.updated", "org:admin")
		if role, keys := state(); role != "member" || keys {
			t.Fatalf("a repeat of Clerk's old role re-promoted them: role=%q keys=%v, want member/false", role, keys)
		}
	})

	t.Run("a real Clerk-side change applies and recomputes the flags", func(t *testing.T) {
		membership("organizationMembership.updated", "org:member") // Clerk really changed: admin -> member
		if role, keys := state(); role != "member" || keys {
			t.Fatalf("after a real Clerk demotion: role=%q keys=%v, want member/false", role, keys)
		}
		membership("organizationMembership.updated", "org:admin") // and back: member -> admin
		if role, keys := state(); role != "admin" || !keys {
			t.Fatalf("a promotion in Clerk left role=%q keys=%v, want admin/true — flags must follow the role", role, keys)
		}
	})
}

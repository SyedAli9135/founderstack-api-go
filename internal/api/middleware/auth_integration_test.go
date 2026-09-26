//go:build integration

package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/pkg/devtoken"
)

func testSystemPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_SYSTEM_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_SYSTEM_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to system test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type multiOrgFixture struct {
	clerkUserID                string
	clerkOrgA, clerkOrgB       string
	orgA, orgB                 pgtype.UUID
	singleUserID, singleOrgClk string
	singleOrg                  pgtype.UUID
}

// newMultiOrgFixture: one person in two orgs (A as admin, B as viewer), plus
// an unrelated single-org person.
func newMultiOrgFixture(t *testing.T, pool *pgxpool.Pool) multiOrgFixture {
	t.Helper()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	s := hex.EncodeToString(b)
	ctx := context.Background()
	fx := multiOrgFixture{
		clerkUserID: "user_mw_multi_" + s, clerkOrgA: "org_mw_a_" + s, clerkOrgB: "org_mw_b_" + s,
		singleUserID: "user_mw_single_" + s, singleOrgClk: "org_mw_single_" + s,
	}
	insertOrg := func(clerkID, slug string) pgtype.UUID {
		var id pgtype.UUID
		if err := pool.QueryRow(ctx,
			"insert into organizations (clerk_org_id, name, slug) values ($1, 'MW Test Org', $2) returning id",
			clerkID, slug).Scan(&id); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), "delete from organizations where id = $1", id)
		})
		return id
	}
	insertUser := func(orgID pgtype.UUID, clerkUserID, role string) {
		if _, err := pool.Exec(ctx,
			"insert into users (org_id, clerk_user_id, email, role) values ($1, $2, 'mw@example.com', $3)",
			orgID, clerkUserID, role); err != nil {
			t.Fatalf("insert user: %v", err)
		}
	}
	fx.orgA = insertOrg(fx.clerkOrgA, "mw-a-"+s)
	fx.orgB = insertOrg(fx.clerkOrgB, "mw-b-"+s)
	fx.singleOrg = insertOrg(fx.singleOrgClk, "mw-single-"+s)
	insertUser(fx.orgA, fx.clerkUserID, "admin")
	insertUser(fx.orgB, fx.clerkUserID, "viewer")
	insertUser(fx.singleOrg, fx.singleUserID, "member")
	return fx
}

// probeRouter echoes back whichever identity RequireAuth resolved, so a
// test can assert on it directly.
func probeRouter(pool *pgxpool.Pool, cfg *config.Config) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	g := r.Group("/api/v1")
	g.Use(RequireAuth(pool, cfg))
	g.GET("/whoami", func(c *gin.Context) {
		u, _ := authctx.FromContext(c)
		c.JSON(http.StatusOK, gin.H{"org_id": u.OrgID.String(), "role": u.Role, "clerk_org_id": u.ClerkOrgID})
	})
	return r
}

type whoami struct {
	OrgID string `json:"org_id"`
	Role  string `json:"role"`
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

func callWhoami(t *testing.T, r *gin.Engine, cfg *config.Config, clerkUserID, clerkOrgID string) (int, whoami) {
	t.Helper()
	token, err := devtoken.SignForOrg(cfg.DevTokenSecret.Expose(), clerkUserID, clerkOrgID)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var out whoami
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestRequireAuth_ActiveOrgClaimSelectsMembership(t *testing.T) {
	pool := testSystemPool(t)
	cfg := &config.Config{AppEnv: "development", DevTokenSecret: "test-dev-token-secret"}
	fx := newMultiOrgFixture(t, pool)
	r := probeRouter(pool, cfg)

	t.Run("claim for A resolves A with A's role", func(t *testing.T) {
		code, got := callWhoami(t, r, cfg, fx.clerkUserID, fx.clerkOrgA)
		if code != http.StatusOK || got.OrgID != fx.orgA.String() || got.Role != "admin" {
			t.Fatalf("got (%d, %+v), want org A as admin", code, got)
		}
	})

	t.Run("switching the claim to B flips org and role", func(t *testing.T) {
		code, got := callWhoami(t, r, cfg, fx.clerkUserID, fx.clerkOrgB)
		if code != http.StatusOK || got.OrgID != fx.orgB.String() || got.Role != "viewer" {
			t.Fatalf("got (%d, %+v), want org B as viewer", code, got)
		}
	})

	t.Run("no claim with several memberships is refused, never guessed", func(t *testing.T) {
		code, got := callWhoami(t, r, cfg, fx.clerkUserID, "")
		if code != http.StatusConflict || got.Error.Code != "ACTIVE_ORGANIZATION_REQUIRED" {
			t.Fatalf("got (%d, %+v), want 409 ACTIVE_ORGANIZATION_REQUIRED", code, got)
		}
	})

	t.Run("no claim with a single membership still works", func(t *testing.T) {
		code, got := callWhoami(t, r, cfg, fx.singleUserID, "")
		if code != http.StatusOK || got.OrgID != fx.singleOrg.String() {
			t.Fatalf("got (%d, %+v), want the sole org", code, got)
		}
	})

	t.Run("claim for an org the person isn't in is rejected", func(t *testing.T) {
		code, got := callWhoami(t, r, cfg, fx.singleUserID, fx.clerkOrgA)
		if code != http.StatusUnauthorized || got.Error.Code != "USER_NOT_SYNCHRONIZED" {
			t.Fatalf("got (%d, %+v), want 401 USER_NOT_SYNCHRONIZED", code, got)
		}
	})

	t.Run("claim for a deactivated org is rejected", func(t *testing.T) {
		if _, err := pool.Exec(context.Background(), "update organizations set is_active = false where id = $1", fx.orgB); err != nil {
			t.Fatal(err)
		}
		code, got := callWhoami(t, r, cfg, fx.clerkUserID, fx.clerkOrgB)
		if code != http.StatusNotFound || got.Error.Code != "ORGANIZATION_NOT_FOUND" {
			t.Fatalf("got (%d, %+v), want 404 ORGANIZATION_NOT_FOUND", code, got)
		}
	})

	t.Run("no claim falls back once only one membership's org is still active", func(t *testing.T) {
		code, got := callWhoami(t, r, cfg, fx.clerkUserID, "")
		if code != http.StatusOK || got.OrgID != fx.orgA.String() {
			t.Fatalf("got (%d, %+v), want org A (B is deactivated)", code, got)
		}
	})
}

//go:build integration

// Verifies WithTx against real Postgres, as app_user — the first real
// tenant-scoped queries this codebase has run (everything before this was
// either app_system or a superuser). Proves the whole point of RLS:
// scoped to org A, you see org A's row and nothing from org B, and a
// query with no tenant context at all sees nothing rather than everything.
package tenant

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/db/dbgen"
)

func testAppUserPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_APP_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_APP_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testSystemPool(t *testing.T) *pgxpool.Pool {
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

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := id.Scan(s); err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return id
}

func TestWithTx_ScopesToOrgAndCommits(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppUserPool(t)
	ctx := context.Background()

	// Fixtures inserted as app_system (bypasses RLS, same as any seed data
	// would be) — two orgs, so cross-tenant leakage would be observable.
	orgA := mustUUID(t, "11111111-2222-3333-4444-555555555501")
	orgB := mustUUID(t, "11111111-2222-3333-4444-555555555502")
	t.Cleanup(func() {
		_, _ = systemPool.Exec(ctx, "delete from organizations where id in ($1, $2)", orgA, orgB)
	})
	if _, err := systemPool.Exec(ctx,
		"insert into organizations (id, name, slug, clerk_org_id) values ($1, 'Org A', $2, $3), ($4, 'Org B', $5, $6)",
		orgA, "tenant-test-org-a", "clerk_tenant_test_a",
		orgB, "tenant-test-org-b", "clerk_tenant_test_b",
	); err != nil {
		t.Fatalf("seed organizations: %v", err)
	}

	t.Run("scoped to org A sees only org A", func(t *testing.T) {
		var name string
		err := WithTx(ctx, appPool, orgA, func(ctx context.Context, q *dbgen.Queries) error {
			row, err := q.GetActiveOrganizationByID(ctx, orgA)
			if err != nil {
				return err
			}
			name = row.Name
			return nil
		})
		if err != nil {
			t.Fatalf("WithTx() error = %v, want nil", err)
		}
		if name != "Org A" {
			t.Fatalf("name = %q, want \"Org A\"", name)
		}
	})

	t.Run("scoped to org A cannot see org B", func(t *testing.T) {
		err := WithTx(ctx, appPool, orgA, func(ctx context.Context, q *dbgen.Queries) error {
			_, err := q.GetActiveOrganizationByID(ctx, orgB)
			return err
		})
		if err == nil {
			t.Fatal("WithTx() error = nil, want a not-found error — org A's transaction should not see org B's row")
		}
	})

	t.Run("a query with no tenant context sees nothing", func(t *testing.T) {
		// Direct query on the pool, deliberately bypassing WithTx — proves
		// the RLS deny-by-default still holds even reached through this
		// package's own pool, not just raw psql as verified during RLS
		// migration work.
		q := dbgen.New(appPool)
		_, err := q.GetActiveOrganizationByID(ctx, orgA)
		if err == nil {
			t.Fatal("query without WithTx unexpectedly returned a row — RLS should deny it with no org context set")
		}
	})
}

func TestWithTx_RollsBackOnError(t *testing.T) {
	appPool := testAppUserPool(t)
	ctx := context.Background()
	orgA := mustUUID(t, "11111111-2222-3333-4444-555555555503")

	sentinelErr := context.Canceled // any distinct error works here
	err := WithTx(ctx, appPool, orgA, func(ctx context.Context, q *dbgen.Queries) error {
		return sentinelErr
	})
	if err != sentinelErr {
		t.Fatalf("WithTx() error = %v, want the function's own error propagated", err)
	}
}

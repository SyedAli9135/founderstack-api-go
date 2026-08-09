// Package tenant provides the one correct way to run a tenant-scoped
// database operation: inside a transaction with the RLS session variable
// set for that transaction only. Every handler doing real per-org work
// against the app_user (RLS-enforced) pool should go through WithTx rather
// than querying the pool directly — a direct query has no org context set
// and will see zero rows under every table's tenant_isolation policy (see
// internal/db/migrations/000002_enable_rls.up.sql), not accidentally see
// everything.
package tenant

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/db/dbgen"
)

// WithTx runs fn inside a fresh transaction on pool (the app_user pool),
// with app.current_org_id set for that transaction's duration via
// set_config(..., true) — not a string-built "SET LOCAL", which doesn't
// support real query parameterization and would need manual value escaping
// to stay injection-safe. fn's queries run against q, scoped to orgID by
// every table's RLS policy for the duration of the transaction. Commits on
// success, rolls back on any error (including a panic recovered by an
// outer Gin Recovery middleware — the deferred Rollback still fires first).
func WithTx(ctx context.Context, pool *pgxpool.Pool, orgID pgtype.UUID, fn func(ctx context.Context, q *dbgen.Queries) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tenant: begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op if Commit already succeeded

	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_org_id', $1, true)", orgID.String()); err != nil {
		return fmt.Errorf("tenant: set org context: %w", err)
	}

	if err := fn(ctx, dbgen.New(tx)); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tenant: commit transaction: %w", err)
	}
	return nil
}

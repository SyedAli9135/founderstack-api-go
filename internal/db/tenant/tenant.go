// Package tenant runs tenant-scoped queries inside a transaction with the
// RLS session variable set for that transaction only. A direct query
// against app_user with no org context set sees zero rows under every
// table's tenant_isolation policy (000002_enable_rls.up.sql), never
// everything — go through WithTx rather than querying the pool directly.
package tenant

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/db/dbgen"
)

// WithTx runs fn inside a fresh transaction on pool (the app_user pool),
// with app.current_org_id set via set_config(..., true) — not a
// string-built "SET LOCAL", which doesn't support real parameterization.
// Commits on success, rolls back on any error or panic.
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

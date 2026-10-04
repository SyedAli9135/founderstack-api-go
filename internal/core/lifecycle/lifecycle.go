// Package lifecycle ends the life of data: purging workspaces deactivated
// longer than their restore window, and expiring records past retention.
package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/core/integrations"
	"github.com/founderstack/api/internal/pkg/safego"
)

// RestoreWindow is how long a deactivated workspace stays restorable; the
// purge job deletes it afterwards. Matches practice.sql's restore guard.
const RestoreWindow = 30 * 24 * time.Hour

const purgeInterval = 6 * time.Hour

// DocPurger deletes one document's vectors, file and rows (documents.Processor).
type DocPurger interface {
	Purge(ctx context.Context, orgID, docID pgtype.UUID) error
}

// Retention is how long each kind of record is kept, in days.
type Retention struct {
	AuditDays, CostDays, StepsDays, ReportsGraceDays, StripeEventsDays int
}

type Purger struct {
	System   *pgxpool.Pool // app_system
	App      *pgxpool.Pool // app_user, for decrypting connections to revoke
	Docs     DocPurger
	Registry *integrations.Registry
	Key      []byte
	Retain   Retention
}

// PurgeOrg permanently deletes a deactivated workspace: external copies first
// (vectors, files, provider grants), then every row, in one database call. A
// failure leaves the workspace in place so the next pass retries.
func (p *Purger) PurgeOrg(ctx context.Context, orgID uuid.UUID) error {
	var active bool
	if err := p.System.QueryRow(ctx, `SELECT is_active FROM organizations WHERE id = $1`, orgID).Scan(&active); err != nil {
		return fmt.Errorf("lifecycle: load org: %w", err)
	}
	if active {
		return fmt.Errorf("lifecycle: org %s is active", orgID)
	}
	pgOrg := pgtype.UUID{Bytes: orgID, Valid: true}

	rows, err := p.System.Query(ctx, `SELECT id FROM documents WHERE org_id = $1`, orgID)
	if err != nil {
		return err
	}
	var docs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		docs = append(docs, id)
	}
	rows.Close()
	for _, d := range docs {
		if err := p.Docs.Purge(ctx, pgOrg, pgtype.UUID{Bytes: d, Valid: true}); err != nil {
			return fmt.Errorf("lifecycle: purge document %s: %w", d, err)
		}
	}

	p.revokeConnections(ctx, pgOrg, orgID)

	if _, err := p.System.Exec(ctx, `SELECT purge_organization($1)`, orgID); err != nil {
		return fmt.Errorf("lifecycle: purge_organization: %w", err)
	}
	slog.Info("lifecycle: purged workspace", "org_id", orgID, "documents", len(docs))
	return nil
}

// revokeConnections asks each provider to invalidate the stored grant, best
// effort: the credentials are deleted regardless, this just closes the grant
// on the provider's side too.
func (p *Purger) revokeConnections(ctx context.Context, pgOrg pgtype.UUID, orgID uuid.UUID) {
	rows, err := p.System.Query(ctx, `SELECT service_name FROM mcp_connections WHERE org_id = $1 AND is_active`, orgID)
	if err != nil {
		return
	}
	var services []string
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil {
			services = append(services, s)
		}
	}
	rows.Close()
	for _, svc := range services {
		prov, ok := p.Registry.Get(svc)
		rev, isRev := prov.(integrations.Revocable)
		if !ok || !isRev {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if tok, err := integrations.GetIntegrationToken(rctx, p.App, p.Key, pgOrg, svc); err == nil {
			_ = rev.RevokeToken(rctx, tok.AccessToken)
		}
		cancel()
	}
}

// dueOrgs lists workspaces whose restore window has passed. A practice waits
// until its client workspaces are gone.
func (p *Purger) dueOrgs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := p.System.Query(ctx, `
		SELECT o.id FROM organizations o
		WHERE o.is_active = false AND o.deactivated_at < now() - make_interval(secs => $1)
		  AND NOT EXISTS (SELECT 1 FROM organizations c WHERE c.parent_practice_id = o.id)
		ORDER BY o.deactivated_at LIMIT 20`, RestoreWindow.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var due []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		due = append(due, id)
	}
	return due, rows.Err()
}

// PurgeDue purges every workspace whose restore window has passed.
func (p *Purger) PurgeDue(ctx context.Context) {
	due, err := p.dueOrgs(ctx)
	if err != nil {
		slog.Error("lifecycle: list due workspaces", "error", err)
		return
	}
	for _, id := range due {
		if err := p.PurgeOrg(ctx, id); err != nil {
			slog.Error("lifecycle: purge workspace failed", "org_id", id, "error", err)
		}
	}
}

// ExpireRecords deletes records past retention, in batches, and returns how many.
func (p *Purger) ExpireRecords(ctx context.Context) int {
	const batch = 5000
	total := 0
	for i := 0; i < 200; i++ { // bounded per pass
		var a, c, s, r, e int
		err := p.System.QueryRow(ctx, `SELECT * FROM purge_expired_records($1,$2,$3,$4,$5,$6)`,
			p.Retain.AuditDays, p.Retain.CostDays, p.Retain.StepsDays, p.Retain.ReportsGraceDays, p.Retain.StripeEventsDays, batch).
			Scan(&a, &c, &s, &r, &e)
		if err != nil {
			slog.Error("lifecycle: expire records", "error", err)
			return total
		}
		n := a + c + s + r + e
		total += n
		if n == 0 {
			break
		}
	}
	if total > 0 {
		slog.Info("lifecycle: expired records past retention", "rows", total)
	}
	return total
}

// Run executes both sweeps at startup and every few hours until ctx ends.
func (p *Purger) Run(ctx context.Context) {
	sweep := func() {
		_ = safego.Do("lifecycle: purge", func() { p.PurgeDue(ctx); p.ExpireRecords(ctx) })
	}
	sweep()
	t := time.NewTicker(purgeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}

package documents

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/db/dbgen"
)

// stuckThreshold is how long a document must have sat in
// pending/processing/deleting before RecoverStuckJobs assumes its
// goroutine was lost (a process restart, e.g.) rather than genuinely
// still running — generous enough that a document actively being
// processed is never falsely re-kicked mid-job.
const stuckThreshold = 10 * time.Minute

// RecoverStuckJobs runs once at startup (see cmd/api/main.go) and
// re-dispatches any document whose processing or purge job was
// abandoned by a process restart — the tradeoff that comes with plain
// goroutines instead of a durable job queue (river, e.g.) for workflow
// 6's background work; see WORKFLOW_PLAN_GO.md workflow 6 for why that
// tradeoff was accepted. Runs on systemPool (BYPASSRLS): scanning across
// every org's documents is inherently cross-tenant, same reasoning as
// internal/core/integrations/refresh.go's RunRefreshJob.
func (p *Processor) RecoverStuckJobs(ctx context.Context, systemPool *pgxpool.Pool) {
	q := dbgen.New(systemPool)

	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-stuckThreshold), Valid: true}
	rows, err := q.ListStuckDocuments(ctx, cutoff)
	if err != nil {
		slog.Error("documents: list stuck documents", "error", err)
		return
	}

	for _, row := range rows {
		status := ""
		if row.ProcessingStatus != nil {
			status = *row.ProcessingStatus
		}
		orgID, docID := row.OrgID, row.ID

		switch status {
		case "deleting":
			go func() {
				if err := p.Purge(ctx, orgID, docID); err != nil {
					slog.Error("documents: recover stuck purge", "doc_id", docID.String(), "error", err)
				}
			}()
		case "pending", "processing":
			go func() {
				if err := p.Process(ctx, orgID, docID); err != nil {
					slog.Error("documents: recover stuck process", "doc_id", docID.String(), "error", err)
				}
			}()
		}
	}

	if len(rows) > 0 {
		slog.Info("documents: recovered stuck jobs", "count", len(rows))
	}
}

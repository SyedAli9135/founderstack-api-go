// Package workflows holds workflow 8's background scheduler — the piece
// that decides *when* a scheduled workflow should fire. It stops at
// inserting a 'pending' workflow_runs row and recomputing next_run_at;
// nothing here calls an LLM, executes a tool, or advances a run past
// 'pending' — that's workflow 9's job (internal/core/graph, not built
// yet). See internal/api/workflows for the CRUD half of workflow 8.
package workflows

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	cron "github.com/robfig/cron/v3"

	"github.com/founderstack/api/internal/db/dbgen"
)

// pollInterval matches workflow 8's spec exactly: a goroutine with
// time.Ticker(60s), not a full cron daemon library running its own
// scheduling loop — robfig/cron/v3 is used here only for ParseStandard's
// validation and Next() computation, not its own Cron{} runner.
const pollInterval = 60 * time.Second

// RunScheduler polls for due scheduled workflows every pollInterval until
// ctx is cancelled. systemPool must be the app_system (BYPASSRLS) pool —
// scanning next_run_at across every org is inherently cross-tenant, same
// reasoning as internal/core/integrations/refresh.go's RunRefreshJob and
// internal/core/documents/recover.go's RecoverStuckJobs. Runs once
// immediately at startup (a workflow whose fire time passed while the
// process was down shouldn't wait a full minute to be caught), then every
// tick thereafter.
func RunScheduler(ctx context.Context, systemPool *pgxpool.Pool) {
	tick(ctx, systemPool)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick(ctx, systemPool)
		}
	}
}

func tick(ctx context.Context, systemPool *pgxpool.Pool) {
	q := dbgen.New(systemPool)

	due, err := q.ListDueScheduledWorkflows(ctx)
	if err != nil {
		slog.Error("workflows: list due scheduled workflows", "error", err)
		return
	}
	if len(due) == 0 {
		return
	}

	for _, wf := range due {
		if err := fireWorkflow(ctx, q, wf); err != nil {
			slog.Error("workflows: fire scheduled workflow", "workflow_id", wf.ID.String(), "error", err)
		}
	}
	slog.Info("workflows: fired due scheduled workflows", "count", len(due))
}

func fireWorkflow(ctx context.Context, q *dbgen.Queries, wf dbgen.ListDueScheduledWorkflowsRow) error {
	if err := q.SystemInsertWorkflowRun(ctx, dbgen.SystemInsertWorkflowRunParams{
		WorkflowID: wf.ID, OrgID: wf.OrgID,
	}); err != nil {
		return err
	}

	if wf.CronExpression == nil {
		// Shouldn't happen — ListDueScheduledWorkflows only returns rows
		// with trigger_type='scheduled', which Create/Update always pair
		// with a non-nil cron_expression — but don't leave next_run_at
		// stuck in the past forever if it somehow does.
		return nil
	}
	schedule, err := cron.ParseStandard(*wf.CronExpression)
	if err != nil {
		return err
	}
	next := schedule.Next(time.Now())
	return q.UpdateWorkflowNextRunAt(ctx, dbgen.UpdateWorkflowNextRunAtParams{
		ID: wf.ID, NextRunAt: pgtype.Timestamptz{Time: next, Valid: true},
	})
}

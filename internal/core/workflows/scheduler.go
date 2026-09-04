// Package workflows decides *when* a scheduled workflow fires. It stops
// at inserting a 'pending' workflow_runs row and recomputing next_run_at
// — nothing here calls an LLM or executes a tool; that's internal/core/graph.
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

// A plain time.Ticker(60s), not a cron daemon — robfig/cron/v3 is used
// only for ParseStandard/Next(), not its own Cron{} runner.
const pollInterval = 60 * time.Second

// systemPool must be app_system (BYPASSRLS): scanning next_run_at across
// every org is inherently cross-tenant. Runs once immediately at startup
// so a workflow whose fire time passed while the process was down isn't
// stuck waiting a full tick.
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

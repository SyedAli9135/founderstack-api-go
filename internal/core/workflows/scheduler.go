// Package workflows decides *when* a scheduled workflow fires, then hands
// the run to the same launcher "Run now" uses — nothing here calls an LLM or
// executes a tool itself; that's internal/core/graph.
package workflows

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	cron "github.com/robfig/cron/v3"

	"github.com/founderstack/api/internal/core/graph"
	"github.com/founderstack/api/internal/db/dbgen"
)

// A plain time.Ticker(60s), not a cron daemon — robfig/cron/v3 is used
// only for ParseStandard/Next(), not its own Cron{} runner.
const pollInterval = 60 * time.Second

// RunLauncher is the slice of *graph.Launcher the scheduler needs — the
// same two calls internal/api/workflows' Run handler makes, so a scheduled
// run goes through exactly the path a manual one does.
type RunLauncher interface {
	Preflight(ctx context.Context, orgID pgtype.UUID) error
	Launch(orgID, agentID, workflowID, runID uuid.UUID, input string)
}

// systemPool must be app_system (BYPASSRLS): scanning next_run_at across
// every org is inherently cross-tenant. Runs once immediately at startup
// so a workflow whose fire time passed while the process was down isn't
// stuck waiting a full tick.
func RunScheduler(ctx context.Context, systemPool *pgxpool.Pool, launcher RunLauncher) {
	tick(ctx, systemPool, launcher)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick(ctx, systemPool, launcher)
		}
	}
}

func tick(ctx context.Context, systemPool *pgxpool.Pool, launcher RunLauncher) {
	reapStaleRuns(ctx, systemPool)

	due, err := dbgen.New(systemPool).ListDueScheduledWorkflows(ctx)
	if err != nil {
		slog.Error("workflows: list due scheduled workflows", "error", err)
		return
	}
	for _, wf := range due {
		if err := fireWorkflow(ctx, systemPool, launcher, wf); err != nil {
			slog.Error("workflows: fire scheduled workflow", "workflow_id", wf.ID.String(), "error", err)
		}
	}
}

// reapStaleRuns fails runs orphaned by a process that died mid-run — without
// it they'd count as active forever and block their schedule's next firing.
func reapStaleRuns(ctx context.Context, systemPool *pgxpool.Pool) {
	reaped, err := dbgen.New(systemPool).ReapStaleRuns(ctx)
	if err != nil {
		slog.Error("workflows: reap stale runs", "error", err)
		return
	}
	for _, r := range reaped {
		slog.Warn("workflows: marked stale run failed", "run_id", r.ID.String(), "org_id", r.OrgID.String())
	}
}

// skipReason is why a claimed firing produced no run; "" means it launched.
type skipReason string

const (
	skipPreflight skipReason = "preflight"
	skipInFlight  skipReason = "previous_run_in_progress"
)

// fireWorkflow claims the workflow under a row lock (so concurrent API
// processes can't both fire it), advances next_run_at, and inserts + launches
// a run — all through the same Preflight/Launch pair "Run now" uses. A slot
// that's skipped (preflight refused, or the previous run is still going)
// still advances next_run_at, so it waits for its next slot instead of
// retrying every tick; only an unexpected error rolls back and leaves it due.
func fireWorkflow(ctx context.Context, systemPool *pgxpool.Pool, launcher RunLauncher, wf dbgen.ListDueScheduledWorkflowsRow) error {
	next, err := nextRunAt(wf.CronExpression)
	if err != nil {
		return err
	}

	var runID pgtype.UUID
	var skipped skipReason
	var claimed bool
	err = pgx.BeginFunc(ctx, systemPool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.ClaimDueScheduledWorkflow(ctx, wf.ID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		claimed = true

		var pe *graph.PreflightError
		if err := launcher.Preflight(ctx, wf.OrgID); err != nil && !errors.As(err, &pe) {
			return err
		}
		if err := q.UpdateWorkflowNextRunAt(ctx, dbgen.UpdateWorkflowNextRunAtParams{ID: wf.ID, NextRunAt: next}); err != nil {
			return err
		}
		if pe != nil {
			skipped = skipPreflight
			slog.Warn("workflows: skipped scheduled run", "workflow_id", wf.ID.String(), "reason", pe.Code)
			return nil
		}
		inFlight, err := q.HasInFlightRun(ctx, wf.ID)
		if err != nil {
			return err
		}
		if inFlight {
			skipped = skipInFlight
			slog.Warn("workflows: skipped scheduled run", "workflow_id", wf.ID.String(), "reason", string(skipInFlight))
			return nil
		}
		runID, err = q.SystemInsertWorkflowRun(ctx, dbgen.SystemInsertWorkflowRunParams{WorkflowID: wf.ID, OrgID: wf.OrgID})
		return err
	})
	if err != nil || !claimed || skipped != "" {
		return err
	}

	var input string
	if wf.TaskInputTemplate != nil {
		input = *wf.TaskInputTemplate
	}
	launcher.Launch(uuid.UUID(wf.OrgID.Bytes), uuid.UUID(wf.AgentID.Bytes), uuid.UUID(wf.ID.Bytes), uuid.UUID(runID.Bytes), input)
	slog.Info("workflows: launched scheduled run", "workflow_id", wf.ID.String(), "run_id", runID.String())
	return nil
}

// nextRunAt is NULL for a missing cron expression — ListDueScheduledWorkflows
// only returns scheduled workflows, which always have one, but a NULL clears
// next_run_at rather than leaving it in the past to re-fire every tick.
func nextRunAt(cronExpr *string) (pgtype.Timestamptz, error) {
	if cronExpr == nil {
		return pgtype.Timestamptz{}, nil
	}
	schedule, err := cron.ParseStandard(*cronExpr)
	if err != nil {
		return pgtype.Timestamptz{}, err
	}
	return pgtype.Timestamptz{Time: schedule.Next(time.Now()), Valid: true}, nil
}

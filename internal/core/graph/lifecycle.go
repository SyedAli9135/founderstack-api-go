package graph

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

// terminalStatuses are the statuses a run never comes back from without a fresh Resume()
var terminalStatuses = map[string]bool{"completed": true, "failed": true, "cancelled": true}

func markRunStarted(ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID) error {
	return tenant.WithTx(ctx, pool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.MarkRunStarted(ctx, dbgen.MarkRunStartedParams{OrgID: orgID, ID: runID})
	})
}

func getRunStatus(ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID) (string, error) {
	var status string
	err := tenant.WithTx(ctx, pool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		status, err = q.GetRunStatus(ctx, dbgen.GetRunStatusParams{OrgID: orgID, ID: runID})
		return err
	})
	return status, err
}

// finalizeIfTerminal fills in the completion-summary fields (output,
// token counts, duration) once a run reaches a terminal status — called
// after both Engine.Run and Engine.Resume return.
func finalizeIfTerminal(ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, state *RunState) error {
	status, err := getRunStatus(ctx, pool, orgID, runID)
	if err != nil {
		return fmt.Errorf("graph: read run status: %w", err)
	}
	if !terminalStatuses[status] {
		return nil
	}

	var output *string
	if state.Output != "" {
		output = &state.Output
	}
	if err := tenant.WithTx(ctx, pool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.FinalizeRun(ctx, dbgen.FinalizeRunParams{
			OrgID: orgID, ID: runID, Output: output,
			InputTokens:  int32(state.TokenUsage.InputTokens),
			OutputTokens: int32(state.TokenUsage.OutputTokens),
			CachedTokens: int32(state.TokenUsage.CachedTokens),
		})
	}); err != nil {
		return err
	}

	// hours_saved only accrues on a genuine success — a failed or
	// cancelled run (and, per this table's own semantics, a rejected
	// approval — the engine still reaches NodeComplete/'completed' there,
	// same as an approved one) delivered no time savings to count.
	if status == "completed" {
		if err := accrueHoursSaved(ctx, pool, orgID, runID); err != nil {
			return fmt.Errorf("graph: accrue hours_saved: %w", err)
		}
	}
	return nil
}

// accrueHoursSaved sets workflow_runs.hours_saved (from the workflow's own
// estimated_manual_minutes, defaulting to 15) and folds it into the org's
// running total — both in one transaction so the two numbers can't drift
// apart if something crashes between them.
func accrueHoursSaved(ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID) error {
	return tenant.WithTx(ctx, pool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		hoursSaved, err := q.FinalizeRunHoursSaved(ctx, dbgen.FinalizeRunHoursSavedParams{OrgID: orgID, ID: runID})
		if err != nil {
			return err
		}
		if hoursSaved == nil {
			return nil
		}
		return q.IncrementOrgTotalHoursSaved(ctx, dbgen.IncrementOrgTotalHoursSavedParams{
			ID: orgID, TotalHoursSaved: *hoursSaved,
		})
	})
}

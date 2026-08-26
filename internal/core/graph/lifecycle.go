package graph

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

// terminalStatuses are the statuses a run never comes back from without
// a fresh Resume() — awaiting_approval is deliberately excluded, since
// that run isn't finished, just paused.
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
// token counts, duration) once — and only once — a run reaches a
// genuinely terminal status. Called by Launcher after Engine.Run
// returns; whatever eventually calls Engine.Resume() for a suspended run
// (workflow 10, not built yet) is responsible for calling this again
// after its own leg finishes, since a resumed run can itself reach a
// terminal status Launcher's own call here never saw.
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
	return tenant.WithTx(ctx, pool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.FinalizeRun(ctx, dbgen.FinalizeRunParams{
			OrgID: orgID, ID: runID, Output: output,
			InputTokens:  int32(state.TokenUsage.InputTokens),
			OutputTokens: int32(state.TokenUsage.OutputTokens),
			CachedTokens: int32(state.TokenUsage.CachedTokens),
		})
	})
}

package graph

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

// checkpoint persists state to workflow_runs.checkpoint_state/current_node
// (plus the running cost/tool-call counters) through a fresh tenant.WithTx
// call — never held open across a node's tool-call loop or an
// approval-gate pause, since WithTx's transaction is meant to be short-lived.
func checkpoint(ctx context.Context, pool *pgxpool.Pool, state *RunState, currentNode NodeName, status string) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("graph: marshal run state: %w", err)
	}

	orgID := pgtype.UUID{Bytes: state.OrgID, Valid: true}
	runID := pgtype.UUID{Bytes: state.WorkflowRunID, Valid: true}
	node := string(currentNode)

	return tenant.WithTx(ctx, pool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.UpdateRunCheckpoint(ctx, dbgen.UpdateRunCheckpointParams{
			OrgID:           orgID,
			ID:              runID,
			CheckpointState: data,
			CurrentNode:     &node,
			CostSoFarUsd:    state.CostSoFarUSD,
			ToolCallCount:   int32(state.ToolCallCount),
			Status:          status,
		})
	})
}

// loadCheckpoint reloads state + current_node for Resume — the
// interrupt()/thread_id replacement.
func loadCheckpoint(ctx context.Context, pool *pgxpool.Pool, orgID, runID uuid.UUID) (*RunState, NodeName, error) {
	orgPg := pgtype.UUID{Bytes: orgID, Valid: true}
	runPg := pgtype.UUID{Bytes: runID, Valid: true}

	var row dbgen.GetRunCheckpointRow
	err := tenant.WithTx(ctx, pool, orgPg, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		row, err = q.GetRunCheckpoint(ctx, dbgen.GetRunCheckpointParams{OrgID: orgPg, ID: runPg})
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("graph: load checkpoint for run %s: %w", runID, err)
	}
	if row.CheckpointState == nil {
		return nil, "", fmt.Errorf("graph: run %s has no checkpoint yet (never started)", runID)
	}

	var state RunState
	if err := json.Unmarshal(row.CheckpointState, &state); err != nil {
		return nil, "", fmt.Errorf("graph: unmarshal checkpoint for run %s: %w", runID, err)
	}

	var node NodeName
	if row.CurrentNode != nil {
		node = NodeName(*row.CurrentNode)
	}
	return &state, node, nil
}

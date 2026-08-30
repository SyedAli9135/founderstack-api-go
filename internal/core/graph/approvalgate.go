package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/founderstack/api/internal/core/llm"
	coremcp "github.com/founderstack/api/internal/core/mcp"
	"github.com/founderstack/api/internal/core/notify"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

// riskRank orders the 3 tiers so batchRiskLevel can pick the single
// highest one a mixed batch should be gated/labeled by — matches
// executorNode's own "the whole batch suspends if any call in it needs
// approval" semantics.
var riskRank = map[coremcp.RiskLevel]int{
	coremcp.RiskRead:                        0,
	coremcp.RiskWriteReversible:             1,
	coremcp.RiskWriteDestructiveOrFinancial: 2,
}

func batchRiskLevel(deps RunDeps, calls []llm.ToolCall) coremcp.RiskLevel {
	highest := coremcp.RiskRead
	for _, tc := range calls {
		level := deps.Tools.RiskLevel(tc.Name)
		if riskRank[level] > riskRank[highest] {
			highest = level
		}
	}
	return highest
}

// writeApprovalGate INSERTs the approvals row a human will actually see
// and decide against, then fires notifications best-effort. Called from
// executorNode right before it returns NodeAwaitingApproval — this INSERT
// is not best-effort: if it fails, the run fails loudly rather than
// suspending with no approvals row anyone could ever act on (the gap this
// workflow closes; nothing wrote to this table before).
func writeApprovalGate(ctx context.Context, deps RunDeps, state *RunState, calls []llm.ToolCall) error {
	riskLevel := string(batchRiskLevel(deps, calls))
	contextData, err := json.Marshal(calls)
	if err != nil {
		return fmt.Errorf("graph: marshal approval context_data: %w", err)
	}
	expiresAt := time.Now().Add(notify.ApprovalTTL)

	runID := pgtype.UUID{Bytes: state.WorkflowRunID, Valid: true}
	var approvalID pgtype.UUID
	err = tenant.WithTx(ctx, deps.AppPool, deps.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		approvalID, err = q.InsertApproval(ctx, dbgen.InsertApprovalParams{
			RunID: runID, OrgID: deps.OrgID, RiskLevel: &riskLevel, ContextData: contextData,
			ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("graph: insert approvals row: %w", err)
	}

	state.ApprovalID = approvalID.Bytes
	state.PendingApprovalRiskLevel = riskLevel

	if deps.Notifier != nil {
		// Detached, not awaited: a slow Slack/email/webpush call must never
		// delay the checkpoint+SSE-publish that follows in engine.go — each
		// channel inside Notifier is independently logged-not-propagated.
		// Takes explicit fields (not RunDeps) so internal/core/notify never
		// needs to import internal/core/graph — this package imports
		// notify, not the other way around.
		go deps.Notifier.NotifyApprovalRequired(context.Background(), deps.AppPool, deps.Gateway, deps.OrgID, approvalID.Bytes, riskLevel, calls)
	}
	return nil
}

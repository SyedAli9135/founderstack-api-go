package workflows

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/core/graph"
	"github.com/founderstack/api/internal/db/dbgen"
)

// 5-minute sweep per spec, not workflow 8's unrelated 60s pollInterval.
const approvalExpiryInterval = 5 * time.Minute

// Without the launcher.Resume call below, an expired approval only flips
// its own approvals.status — workflow_runs stays stuck at
// 'awaiting_approval' forever, since nothing else tells its state
// machine the wait is over.
func RunApprovalExpiryJob(ctx context.Context, systemPool *pgxpool.Pool, launcher *graph.Launcher) {
	expireApprovals(ctx, systemPool, launcher)

	ticker := time.NewTicker(approvalExpiryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			expireApprovals(ctx, systemPool, launcher)
		}
	}
}

func expireApprovals(ctx context.Context, systemPool *pgxpool.Pool, launcher *graph.Launcher) {
	q := dbgen.New(systemPool)

	expired, err := q.ListExpiredPendingApprovals(ctx)
	if err != nil {
		slog.Error("workflows: list expired pending approvals", "error", err)
		return
	}
	if len(expired) == 0 {
		return
	}

	const reason = "Approval expired after 24h with no decision"
	status := "expired"
	for _, approval := range expired {
		if _, err := q.UpdateApprovalStatus(ctx, dbgen.UpdateApprovalStatusParams{
			OrgID: approval.OrgID, ID: approval.ID, Status: &status,
		}); err != nil {
			slog.Error("workflows: expire approval", "approval_id", approval.ID.String(), "error", err)
			continue
		}
		reasonCopy := reason
		if err := q.InsertApprovalDecision(ctx, dbgen.InsertApprovalDecisionParams{
			ApprovalID: approval.ID, UserID: pgtype.UUID{}, Decision: status, Reason: &reasonCopy,
		}); err != nil {
			slog.Error("workflows: insert expiry decision", "approval_id", approval.ID.String(), "error", err)
		}
		launcher.Resume(uuid.UUID(approval.OrgID.Bytes), uuid.UUID(approval.RunID.Bytes), false, reason)
	}
	slog.Info("workflows: expired pending approvals", "count", len(expired))
}

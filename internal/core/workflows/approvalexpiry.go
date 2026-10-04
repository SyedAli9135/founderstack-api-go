package workflows

import (
	"context"
	"github.com/founderstack/api/internal/pkg/safego"
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
	_ = safego.Do("workflows: approval expiry", func() { expireApprovals(ctx, systemPool, launcher) })

	ticker := time.NewTicker(approvalExpiryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = safego.Do("workflows: approval expiry", func() { expireApprovals(ctx, systemPool, launcher) })
		}
	}
}

// resumer is the one thing the sweep needs from the launcher.
type resumer interface {
	Resume(orgID, runID uuid.UUID, approved bool, reason string)
}

const expiryReason = "Approval expired after 24h with no decision"

func expireApprovals(ctx context.Context, systemPool *pgxpool.Pool, launcher resumer) {
	q := dbgen.New(systemPool)

	expired, err := q.ListExpiredPendingApprovals(ctx)
	if err != nil {
		slog.Error("workflows: list expired pending approvals", "error", err)
		return
	}
	done := 0
	for _, approval := range expired {
		if expireOne(ctx, q, launcher, approval) {
			done++
		}
	}
	if done > 0 {
		slog.Info("workflows: expired pending approvals", "count", done)
	}
}

// expireOne expires one approval and resumes its run, only if this call is the
// one that flipped it out of 'pending'. The row was listed a moment ago; a
// person may have decided it since, or another instance's sweep may have got
// there first — resuming the run again then would drive it twice.
func expireOne(ctx context.Context, q *dbgen.Queries, launcher resumer, approval dbgen.ListExpiredPendingApprovalsRow) bool {
	status := "expired"
	n, err := q.UpdateApprovalStatus(ctx, dbgen.UpdateApprovalStatusParams{OrgID: approval.OrgID, ID: approval.ID, Status: &status})
	if err != nil {
		slog.Error("workflows: expire approval", "approval_id", approval.ID.String(), "error", err)
		return false
	}
	if n == 0 {
		return false
	}
	reason := expiryReason
	if err := q.InsertApprovalDecision(ctx, dbgen.InsertApprovalDecisionParams{
		ApprovalID: approval.ID, UserID: pgtype.UUID{}, Decision: status, Reason: &reason,
	}); err != nil {
		slog.Error("workflows: insert expiry decision", "approval_id", approval.ID.String(), "error", err)
	}
	launcher.Resume(uuid.UUID(approval.OrgID.Bytes), uuid.UUID(approval.RunID.Bytes), false, expiryReason)
	return true
}

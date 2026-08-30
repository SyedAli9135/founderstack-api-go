-- Workflow 10: approvals/approval_decisions writes (internal/core/graph's
-- writeApprovalGate, internal/api/approvals' handler), notification lookups
-- (internal/core/notify), and the 24h expiry sweep
-- (internal/core/workflows/approvalexpiry.go). InsertApproval/GetApproval/
-- ListApprovalsForOrg/UpdateApprovalStatus/InsertApprovalDecision run
-- through tenant.WithTx (app_user) except ListExpiredPendingApprovals,
-- which is inherently cross-org and runs on app_system, same reasoning as
-- every other background sweep in this codebase.

-- name: InsertApproval :one
INSERT INTO approvals (run_id, org_id, status, risk_level, context_data, expires_at)
VALUES ($1, $2, 'pending', $3, $4, $5)
RETURNING id;

-- name: GetApproval :one
SELECT id, run_id, org_id, status, risk_level, context_data, expires_at, created_at
FROM approvals
WHERE org_id = $1 AND id = $2;

-- GetApprovalSystemScoped runs on app_system (BYPASSRLS) — the
-- action-token approve/reject path (internal/api/approvals/handler.go)
-- has no tenant context yet at this point (an unauthenticated caller
-- can't SET LOCAL app.current_org_id for an org it hasn't proven it
-- belongs to), the same chicken-and-egg reasoning as
-- internal/api/middleware/auth.go's RequireAuth resolving identity before
-- any org context exists. Scoped by primary key only; every write that
-- follows still goes through the normal org-scoped path once org_id is
-- known from this row.
-- name: GetApprovalSystemScoped :one
SELECT id, run_id, org_id, status, risk_level, context_data, expires_at, created_at
FROM approvals
WHERE id = $1;

-- name: ListApprovalsForOrg :many
SELECT id, run_id, org_id, status, risk_level, context_data, expires_at, created_at
FROM approvals
WHERE org_id = $1
  AND (sqlc.narg(status)::varchar IS NULL OR status = sqlc.narg(status))
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: UpdateApprovalStatus :execrows
UPDATE approvals SET status = $3
WHERE org_id = $1 AND id = $2 AND status = 'pending';

-- name: InsertApprovalDecision :exec
INSERT INTO approval_decisions (approval_id, user_id, decision, reason)
VALUES ($1, $2, $3, $4);

-- ListExpiredPendingApprovals runs on app_system (BYPASSRLS) — sweeping
-- expirations across every org is inherently cross-tenant, same reasoning
-- as internal/core/integrations/refresh.go's RunRefreshJob.
-- name: ListExpiredPendingApprovals :many
SELECT id, run_id, org_id FROM approvals
WHERE status = 'pending' AND expires_at IS NOT NULL AND expires_at < now();

-- name: GetOrgApprovalsSlackChannel :one
SELECT approvals_slack_channel_id FROM organizations WHERE id = $1;

-- name: UpdateOrgApprovalsSlackChannel :exec
UPDATE organizations SET approvals_slack_channel_id = $2 WHERE id = $1;

-- name: UpsertPushSubscription :exec
INSERT INTO push_subscriptions (org_id, user_id, endpoint, p256dh_key, auth_key)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (org_id, user_id, endpoint) DO UPDATE
SET p256dh_key = EXCLUDED.p256dh_key, auth_key = EXCLUDED.auth_key;

-- name: DeletePushSubscription :exec
DELETE FROM push_subscriptions WHERE org_id = $1 AND user_id = $2 AND endpoint = $3;

-- name: ListPushSubscriptionsForOrg :many
SELECT id, user_id, endpoint, p256dh_key, auth_key FROM push_subscriptions WHERE org_id = $1;

-- name: ListApproverEmailsForOrg :many
SELECT id, email FROM users
WHERE org_id = $1 AND can_approve_workflows = true AND is_active = true;

-- GetUserApprovalPermissions backs both the authenticated and the
-- action-token approve/reject paths (internal/api/approvals/handler.go) —
-- neither authctx.User nor a Clerk JWT carries can_approve_workflows, so
-- this is looked up fresh at decision time rather than cached on the
-- request context.
-- name: GetUserApprovalPermissions :one
SELECT id, org_id, email, can_approve_workflows FROM users
WHERE id = $1 AND is_active = true;

-- agents_paused reuses graph.sql's GetOrgRunSettings (same organizations
-- row workflow 9's Preflight already reads) rather than a second query for
-- the same column.

-- Workflow 21 (Practice & Client Workspace Model). Every query here runs on
-- app_system (BYPASSRLS), because a portfolio view is inherently
-- cross-tenant — RLS can only ever scope to one org at a time. What stands
-- in for RLS is the users join every read carries: a workspace is only ever
-- visible to a caller holding an active users row in that exact workspace
-- (clerk_user_id = the verified JWT subject). Don't add a query to this file
-- without that join.

-- name: ListWorkspacesForClerkUser :many
-- Backs the workspace switcher: every active org the person belongs to.
SELECT o.id, o.clerk_org_id, o.name, o.slug, o.organization_type, o.parent_practice_id, u.role
FROM organizations o
JOIN users u ON u.org_id = o.id AND u.clerk_user_id = $1 AND u.is_active = true
WHERE o.is_active = true
ORDER BY (o.organization_type = 'client_workspace'), o.name;

-- name: GetPracticeForUpdate :one
-- Row lock serializes concurrent creates against the same practice, so two
-- simultaneous requests can't both pass the max_client_workspaces check.
SELECT id, organization_type, max_client_workspaces
FROM organizations WHERE id = $1 AND is_active = true
FOR UPDATE;

-- name: CountActiveClientWorkspaces :one
SELECT count(*) FROM organizations WHERE parent_practice_id = $1 AND is_active = true;

-- name: MarkOrganizationAsPractice :exec
-- A standard org becomes a practice the first time it creates a client workspace.
UPDATE organizations SET organization_type = 'practice'
WHERE id = $1 AND organization_type = 'standard';

-- name: UpsertClientWorkspace :one
-- ON CONFLICT covers Clerk's organization.created webhook racing ahead of
-- this insert: the webhook creates a plain standard row, and this turns it
-- into the client workspace it actually is.
INSERT INTO organizations (clerk_org_id, name, slug, organization_type, parent_practice_id, settings, is_active)
VALUES ($1, $2, $3, 'client_workspace', $4, $5, true)
ON CONFLICT (clerk_org_id) DO UPDATE SET
    organization_type = 'client_workspace',
    parent_practice_id = EXCLUDED.parent_practice_id,
    settings = EXCLUDED.settings
RETURNING id;

-- name: ListClientWorkspacesWithStats :many
-- Includes deactivated workspaces (still inside or past their restore
-- window) so the portfolio page can offer a restore; the handler decides
-- restorability from deactivated_at.
SELECT
    o.id, o.clerk_org_id, o.name, o.slug, o.is_active, o.deactivated_at, o.created_at,
    o.settings,
    o.total_hours_saved::double precision AS hours_saved,
    (SELECT count(*) FROM workflow_runs wr
        WHERE wr.org_id = o.id AND wr.parent_run_id IS NULL
          AND wr.status IN ('pending', 'running', 'awaiting_approval'))::bigint AS active_runs,
    (SELECT count(*) FROM approvals ap
        WHERE ap.org_id = o.id AND ap.status = 'pending')::bigint AS pending_approvals,
    (SELECT COALESCE(SUM(cl.estimated_cost_usd), 0) FROM cost_ledger cl
        WHERE cl.org_id = o.id)::double precision AS total_cost_usd
FROM organizations o
JOIN users u ON u.org_id = o.id AND u.clerk_user_id = $2 AND u.is_active = true
WHERE o.parent_practice_id = $1
ORDER BY o.is_active DESC, o.name;

-- name: GetPortfolioSummary :one
SELECT
    count(*)::bigint AS active_workspaces,
    COALESCE(SUM(o.total_hours_saved), 0)::double precision AS hours_saved,
    COALESCE(SUM((SELECT count(*) FROM workflow_runs wr
        WHERE wr.org_id = o.id AND wr.parent_run_id IS NULL
          AND wr.status IN ('pending', 'running', 'awaiting_approval'))), 0)::bigint AS active_runs,
    COALESCE(SUM((SELECT count(*) FROM approvals ap
        WHERE ap.org_id = o.id AND ap.status = 'pending')), 0)::bigint AS pending_approvals,
    COALESCE(SUM((SELECT COALESCE(SUM(cl.estimated_cost_usd), 0) FROM cost_ledger cl
        WHERE cl.org_id = o.id)), 0)::double precision AS total_cost_usd
FROM organizations o
JOIN users u ON u.org_id = o.id AND u.clerk_user_id = $2 AND u.is_active = true
WHERE o.parent_practice_id = $1 AND o.is_active = true;

-- name: GetClientWorkspaceForCaller :one
SELECT o.id, o.clerk_org_id, o.name, o.is_active, o.deactivated_at
FROM organizations o
JOIN users u ON u.org_id = o.id AND u.clerk_user_id = $3 AND u.is_active = true
WHERE o.id = $1 AND o.parent_practice_id = $2;

-- name: DeactivateClientWorkspace :execrows
UPDATE organizations SET is_active = false, deactivated_at = now()
WHERE id = $1 AND parent_practice_id = $2 AND is_active = true;

-- name: RestoreClientWorkspace :execrows
UPDATE organizations SET is_active = true, deactivated_at = NULL
WHERE id = $1 AND parent_practice_id = $2 AND is_active = false
  AND deactivated_at > now() - interval '30 days';

-- name: GetPracticeLimit :one
SELECT max_client_workspaces FROM organizations WHERE id = $1;

-- name: GetUserProfile :one
-- Copies the caller's own profile onto the membership row created for a new
-- client workspace (users.email is NOT NULL; the session JWT doesn't carry it).
SELECT email, full_name FROM users WHERE id = $1;

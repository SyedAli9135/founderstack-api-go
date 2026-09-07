-- Workflow 13 (team members & roles). Postgres — kept in sync by the Clerk
-- webhook (clerk_sync.sql) — is this app's own source of truth for role
-- display; internal/api/org/handler.go doesn't call out to Clerk's API on
-- every list, only on a role change or removal (see that package's own
-- doc comment for why).

-- name: ListOrgMembers :many
SELECT id, clerk_user_id, email, full_name, avatar_url, role,
       can_manage_api_keys, can_manage_integrations, can_approve_workflows,
       last_login_at, created_at
FROM users
WHERE org_id = $1 AND is_active = true
ORDER BY created_at ASC;

-- name: GetOrgMemberForUpdate :one
-- Confirms the target member belongs to the caller's own org (cross-org
-- access is a 404 here, same "wrong org is indistinguishable from doesn't
-- exist" convention as every other tenant-scoped lookup in this codebase)
-- and returns clerk_user_id, needed for the Clerk-side sync call.
SELECT id, clerk_user_id, role FROM users
WHERE org_id = $1 AND id = $2 AND is_active = true;

-- name: UpdateMemberRoleAndPermissions :exec
-- Permission flags are recomputed from the new role (see
-- internal/api/org/handler.go's defaultPermissionsForRole), not passed
-- through as independent client input — the schema's can_manage_* columns
-- exist for other code (can_approve_workflows is already read directly by
-- internal/api/approvals) to check without needing to know this app's role
-- hierarchy, but they're derived, not separately settable.
UPDATE users
SET role = $3, can_manage_api_keys = $4, can_manage_integrations = $5, can_approve_workflows = $6
WHERE org_id = $1 AND id = $2;

-- name: DeactivateMember :exec
-- Only ever called after the real Clerk-side removal already succeeded —
-- see Handler.Remove's doc comment for why the ordering matters.
UPDATE users SET is_active = false WHERE org_id = $1 AND id = $2;

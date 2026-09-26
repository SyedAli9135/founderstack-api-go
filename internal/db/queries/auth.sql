-- Queries backing request authentication (internal/api/middleware/auth.go).
-- Run through app_system (BYPASSRLS): resolving "who is this JWT for, and
-- which org do they belong to" is inherently a lookup that happens before
-- any tenant context exists to scope an RLS-restricted query by — the same
-- chicken-and-egg reasoning as the Clerk webhook's org creation.

-- name: GetActiveUserInOrg :one
-- A person can hold one users row per org, so identity is always the
-- (org_id, clerk_user_id) pair, never clerk_user_id alone.
SELECT id, org_id, role, can_manage_api_keys, can_manage_integrations
FROM users WHERE org_id = $1 AND clerk_user_id = $2 AND is_active = true;

-- name: ListActiveMembershipsByClerkUserID :many
-- Fallback for a token carrying no active-org claim: only unambiguous when
-- exactly one membership is in a still-active org.
SELECT u.org_id, COALESCE(o.is_active, false)::boolean AS org_is_active
FROM users u
JOIN organizations o ON o.id = u.org_id
WHERE u.clerk_user_id = $1 AND u.is_active = true;

-- name: GetActiveOrganizationByID :one
SELECT id, name, slug, clerk_org_id, organization_type, parent_practice_id
FROM organizations WHERE id = $1 AND is_active = true;

-- name: GetActiveOrganizationByClerkOrgID :one
SELECT id, name, slug, clerk_org_id, organization_type, parent_practice_id
FROM organizations WHERE clerk_org_id = $1 AND is_active = true;

-- name: TouchLastLogin :exec
-- Best-effort, fire-and-forget from RequireAuth — the WHERE guard keeps
-- this to one write per user per 5 minutes, not one per request.
UPDATE users SET last_login_at = now()
WHERE id = $1 AND (last_login_at IS NULL OR last_login_at < now() - interval '5 minutes');

-- Queries backing request authentication (internal/api/middleware/auth.go).
-- Run through app_system (BYPASSRLS): resolving "who is this JWT for, and
-- which org do they belong to" is inherently a lookup that happens before
-- any tenant context exists to scope an RLS-restricted query by — the same
-- chicken-and-egg reasoning as the Clerk webhook's org creation.

-- name: GetActiveUserByClerkUserID :one
SELECT id, org_id, role, can_manage_api_keys, can_manage_integrations
FROM users WHERE clerk_user_id = $1 AND is_active = true;

-- name: GetActiveOrganizationByID :one
SELECT id, name, slug, clerk_org_id FROM organizations WHERE id = $1 AND is_active = true;

-- name: TouchLastLogin :exec
-- Best-effort, fire-and-forget from RequireAuth — the WHERE guard keeps
-- this to one write per user per 5 minutes, not one per request.
UPDATE users SET last_login_at = now()
WHERE id = $1 AND (last_login_at IS NULL OR last_login_at < now() - interval '5 minutes');

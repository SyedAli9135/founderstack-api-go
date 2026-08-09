-- Queries backing request authentication (internal/api/middleware/auth.go).
-- Run through app_system (BYPASSRLS): resolving "who is this JWT for, and
-- which org do they belong to" is inherently a lookup that happens before
-- any tenant context exists to scope an RLS-restricted query by — the same
-- chicken-and-egg reasoning as the Clerk webhook's org creation.

-- name: GetActiveUserByClerkUserID :one
SELECT id, org_id, role FROM users WHERE clerk_user_id = $1 AND is_active = true;

-- name: GetActiveOrganizationByID :one
SELECT id, name, slug FROM organizations WHERE id = $1 AND is_active = true;

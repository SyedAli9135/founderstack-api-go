-- Queries backing the Clerk webhook sync (POST /api/webhooks/clerk). Run
-- through the app_system (BYPASSRLS) pool, never app_user — see
-- internal/api/webhooks/clerk.go.

-- name: UpsertOrganization :one
INSERT INTO organizations (clerk_org_id, name, slug, is_active)
VALUES ($1, $2, $3, true)
ON CONFLICT (clerk_org_id) DO UPDATE SET
    name = EXCLUDED.name,
    slug = EXCLUDED.slug,
    is_active = true
RETURNING id;

-- name: GetOrganizationIDByClerkOrgID :one
SELECT id FROM organizations WHERE clerk_org_id = $1;

-- can_approve_workflows/can_manage_api_keys/can_manage_integrations are
-- reset to the freshly computed role-derived default (EXCLUDED) only when
-- the existing row is currently inactive — a genuine new membership after
-- having been removed. An already-active member's flags are left
-- untouched on conflict, since those may since have been hand-adjusted via
-- workflow 13's PATCH .../role, which a routine Clerk membership re-sync
-- (role unchanged, just metadata) must never silently clobber.
--
-- Real bug this fixes, found live 2026-09-07: without the is_active
-- branch, a former admin/owner who was removed and later re-invited as a
-- plain member kept their old admin-era flags forever — the ON CONFLICT
-- branch never touched these 3 columns at all, so ANY re-sync (including
-- a brand-new membership) silently carried forward whatever a completely
-- unrelated, already-terminated membership had left behind.
-- name: UpsertUserForMembership :exec
INSERT INTO users (org_id, clerk_user_id, email, full_name, role, can_approve_workflows, can_manage_api_keys, can_manage_integrations, is_active)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true)
ON CONFLICT (clerk_user_id) DO UPDATE SET
    org_id = EXCLUDED.org_id,
    role = EXCLUDED.role,
    is_active = true,
    can_approve_workflows = CASE WHEN users.is_active THEN users.can_approve_workflows ELSE EXCLUDED.can_approve_workflows END,
    can_manage_api_keys = CASE WHEN users.is_active THEN users.can_manage_api_keys ELSE EXCLUDED.can_manage_api_keys END,
    can_manage_integrations = CASE WHEN users.is_active THEN users.can_manage_integrations ELSE EXCLUDED.can_manage_integrations END;

-- name: UpdateUserProfile :execrows
UPDATE users SET full_name = $2, avatar_url = $3 WHERE clerk_user_id = $1;

-- name: SoftDeleteOrganizationByClerkOrgID :execrows
UPDATE organizations SET is_active = false WHERE clerk_org_id = $1;

-- name: SoftDeleteUserByClerkUserID :execrows
UPDATE users SET is_active = false WHERE clerk_user_id = $1;

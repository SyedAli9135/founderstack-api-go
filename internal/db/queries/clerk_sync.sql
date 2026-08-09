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

-- name: UpsertUserForMembership :exec
INSERT INTO users (org_id, clerk_user_id, email, full_name, role, is_active)
VALUES ($1, $2, $3, $4, $5, true)
ON CONFLICT (clerk_user_id) DO UPDATE SET
    org_id = EXCLUDED.org_id,
    role = EXCLUDED.role,
    is_active = true;

-- name: UpdateUserProfile :execrows
UPDATE users SET full_name = $2, avatar_url = $3 WHERE clerk_user_id = $1;

-- name: SoftDeleteOrganizationByClerkOrgID :execrows
UPDATE organizations SET is_active = false WHERE clerk_org_id = $1;

-- name: SoftDeleteUserByClerkUserID :execrows
UPDATE users SET is_active = false WHERE clerk_user_id = $1;

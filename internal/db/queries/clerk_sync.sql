-- Queries backing the Clerk webhook sync (POST /api/webhooks/clerk). Run
-- through the app_system (BYPASSRLS) pool, never app_user — see
-- internal/api/webhooks/clerk.go.

-- name: UpsertOrganization :one
INSERT INTO organizations (clerk_org_id, name, slug, is_active)
VALUES ($1, $2, $3, true)
-- is_active is deliberately left alone on conflict: an organization.updated
-- (or a late-delivered organization.created) must never resurrect an org
-- this backend has deactivated — e.g. a removed client workspace.
-- The webhook substitutes a clerk_org_id-derived slug when Clerk sends none
-- (org slugs disabled on the instance); that stand-in must never replace a
-- real slug already stored, e.g. one a client workspace was created with.
ON CONFLICT (clerk_org_id) DO UPDATE SET
    name = EXCLUDED.name,
    slug = CASE WHEN sqlc.arg(slug_from_clerk)::boolean THEN EXCLUDED.slug ELSE organizations.slug END
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
-- name: UpsertUserForMembership :execrows
-- event_at is the Clerk event's own timestamp (NULL when unknown, e.g. a row
-- written by workspace creation): an event older than the newest one already
-- applied to this membership changes nothing, so a retried "created" can't
-- undo a later removal and a stale "updated" can't revert a role.
--
-- clerk_role is the role Clerk reports. users.role (what the app enforces) is
-- replaced only when Clerk's role changed or the membership is new/reactivated;
-- the permission flags are recomputed in exactly those cases, so a promotion
-- made in Clerk doesn't leave an admin with a member's flags. An unrelated
-- membership event leaves both alone, so an app-side demotion sticks.
INSERT INTO users (org_id, clerk_user_id, email, full_name, role, clerk_role, can_approve_workflows, can_manage_api_keys, can_manage_integrations, is_active, clerk_event_at)
VALUES (sqlc.arg(org_id), sqlc.arg(clerk_user_id), sqlc.arg(email), sqlc.arg(full_name), sqlc.arg(role), sqlc.arg(role), sqlc.arg(can_approve_workflows), sqlc.arg(can_manage_api_keys), sqlc.arg(can_manage_integrations), true, sqlc.narg(event_at)::timestamptz)
ON CONFLICT (org_id, clerk_user_id) DO UPDATE SET
    role = CASE WHEN NOT users.is_active OR users.clerk_role IS DISTINCT FROM EXCLUDED.clerk_role THEN EXCLUDED.role ELSE users.role END,
    can_approve_workflows = CASE WHEN NOT users.is_active OR users.clerk_role IS DISTINCT FROM EXCLUDED.clerk_role THEN EXCLUDED.can_approve_workflows ELSE users.can_approve_workflows END,
    can_manage_api_keys = CASE WHEN NOT users.is_active OR users.clerk_role IS DISTINCT FROM EXCLUDED.clerk_role THEN EXCLUDED.can_manage_api_keys ELSE users.can_manage_api_keys END,
    can_manage_integrations = CASE WHEN NOT users.is_active OR users.clerk_role IS DISTINCT FROM EXCLUDED.clerk_role THEN EXCLUDED.can_manage_integrations ELSE users.can_manage_integrations END,
    clerk_role = EXCLUDED.clerk_role,
    is_active = true,
    clerk_event_at = GREATEST(users.clerk_event_at, EXCLUDED.clerk_event_at)
WHERE users.clerk_event_at IS NULL OR EXCLUDED.clerk_event_at IS NULL OR EXCLUDED.clerk_event_at >= users.clerk_event_at;

-- name: UpdateUserProfile :execrows
-- Profile fields are per-person, so this deliberately updates every
-- membership row the person holds, across all their orgs.
UPDATE users SET full_name = $2, avatar_url = $3 WHERE clerk_user_id = $1;

-- name: SoftDeleteOrganizationByClerkOrgID :execrows
UPDATE organizations SET is_active = false, deactivated_at = COALESCE(deactivated_at, now()) WHERE clerk_org_id = $1;

-- name: SoftDeleteUserByClerkUserID :execrows
-- A full Clerk account deletion (user.deleted): every membership goes.
UPDATE users SET is_active = false, clerk_event_at = GREATEST(clerk_event_at, sqlc.narg(event_at)::timestamptz)
WHERE clerk_user_id = sqlc.arg(clerk_user_id)
  AND (clerk_event_at IS NULL OR sqlc.narg(event_at)::timestamptz IS NULL OR sqlc.narg(event_at)::timestamptz >= clerk_event_at);

-- name: EraseUserByClerkUserID :execrows
-- Account deletion (user.deleted) is final, so unlike a membership removal it
-- also scrubs the person's identifying fields. The row stays (audit history,
-- run attribution) but no longer says who it was. A stale event (older than
-- the newest applied to the row) is ignored, like every other membership write.
WITH erased AS (
    UPDATE users SET is_active = false, can_manage_api_keys = false, can_manage_integrations = false,
           can_approve_workflows = false, full_name = NULL, avatar_url = NULL,
           email = 'erased-' || id::text || '@erased.invalid',
           clerk_user_id = 'erased:' || id::text,
           clerk_event_at = GREATEST(clerk_event_at, sqlc.narg(event_at)::timestamptz)
    WHERE clerk_user_id = sqlc.arg(clerk_user_id)
      AND (clerk_event_at IS NULL OR sqlc.narg(event_at)::timestamptz IS NULL OR sqlc.narg(event_at)::timestamptz >= clerk_event_at)
    RETURNING id
), subs AS (
    DELETE FROM push_subscriptions WHERE user_id IN (SELECT id FROM erased)
)
SELECT 1 FROM erased;

-- name: SoftDeleteMembership :execrows
-- A single membership removal: only this org's row, never the person's
-- memberships elsewhere.
UPDATE users SET is_active = false, clerk_event_at = GREATEST(clerk_event_at, sqlc.narg(event_at)::timestamptz)
WHERE clerk_user_id = sqlc.arg(clerk_user_id)
  AND org_id = (SELECT id FROM organizations WHERE clerk_org_id = sqlc.arg(clerk_org_id))
  AND (clerk_event_at IS NULL OR sqlc.narg(event_at)::timestamptz IS NULL OR sqlc.narg(event_at)::timestamptz >= clerk_event_at);

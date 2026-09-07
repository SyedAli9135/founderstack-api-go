-- Workflow 13 introduces the first real enforcement of can_manage_api_keys/
-- can_manage_integrations (internal/api/settings/apikey.go). Neither flag
-- was ever set to true by any code path before this workflow's webhook fix
-- (internal/api/webhooks/clerk.go's upsertMembership) -- every existing
-- admin/owner user, including a real org's own founder, currently has both
-- false. Enforcing the new gate without this backfill would immediately
-- lock every existing admin/owner out of their own BYOK settings -- the
-- exact same class of gap workflow 10's can_approve_workflows fix closed
-- for existing users (see WORKFLOW_PLAN_GO.md's Workflow 10 section).
UPDATE users
SET can_manage_api_keys = true, can_manage_integrations = true
WHERE role IN ('admin', 'owner') AND is_active = true;

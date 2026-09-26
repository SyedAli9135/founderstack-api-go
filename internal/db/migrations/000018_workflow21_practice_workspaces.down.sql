-- Fails if any person already holds rows in 2+ orgs -- that data can't be
-- represented under the old single-org constraint, and silently deleting
-- memberships to make it fit would be worse than failing loudly.
DROP INDEX IF EXISTS idx_users_clerk_user_id;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_org_id_clerk_user_id_key;
ALTER TABLE users ADD CONSTRAINT users_clerk_user_id_key UNIQUE (clerk_user_id);

DROP INDEX IF EXISTS idx_organizations_parent_practice_id;
ALTER TABLE organizations DROP COLUMN IF EXISTS deactivated_at;
ALTER TABLE organizations DROP COLUMN IF EXISTS max_client_workspaces;
ALTER TABLE organizations DROP CONSTRAINT IF EXISTS organizations_client_workspace_parent_check;
ALTER TABLE organizations DROP COLUMN IF EXISTS organization_type;
ALTER TABLE organizations DROP COLUMN IF EXISTS parent_practice_id;

ALTER TABLE organizations ALTER COLUMN max_client_workspaces SET DEFAULT 5;
ALTER TABLE organizations DROP COLUMN IF EXISTS included_client_workspaces;

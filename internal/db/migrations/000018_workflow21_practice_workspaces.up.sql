-- Workflow 21 (Practice & Client Workspace Model). A Practice is a top-level
-- organizations row; a Client Workspace is an organizations row whose
-- parent_practice_id points at its Practice. Each stays its own RLS tenant --
-- nothing about tenant_isolation changes.
ALTER TABLE organizations ADD COLUMN parent_practice_id uuid REFERENCES organizations(id);
ALTER TABLE organizations ADD COLUMN organization_type varchar(20) NOT NULL DEFAULT 'standard'
    CHECK (organization_type IN ('standard', 'practice', 'client_workspace'));
-- One-level hierarchy only: a client workspace always has a parent, nothing
-- else ever does.
ALTER TABLE organizations ADD CONSTRAINT organizations_client_workspace_parent_check
    CHECK ((organization_type = 'client_workspace') = (parent_practice_id IS NOT NULL));
-- Placeholder cap until workflow 24 ties it to a real plan tier.
ALTER TABLE organizations ADD COLUMN max_client_workspaces integer NOT NULL DEFAULT 5;
-- Set when a client workspace is removed; the restore window is measured from it.
ALTER TABLE organizations ADD COLUMN deactivated_at timestamptz;
CREATE INDEX idx_organizations_parent_practice_id ON organizations(parent_practice_id)
    WHERE parent_practice_id IS NOT NULL;

-- One person can now hold a users row in many orgs. Pure relaxation of the
-- old constraint, so existing data always satisfies the new one.
ALTER TABLE users DROP CONSTRAINT users_clerk_user_id_key;
ALTER TABLE users ADD CONSTRAINT users_org_id_clerk_user_id_key UNIQUE (org_id, clerk_user_id);
CREATE INDEX idx_users_clerk_user_id ON users(clerk_user_id);

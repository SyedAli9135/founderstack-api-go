-- Workflow 23 (Client-Facing Reports & Sharing). A report is a frozen
-- snapshot of one workspace's activity over a date window, shared by an
-- unguessable link that needs no login. snapshot holds only the sections
-- visible_sections allowed at generation time: hidden cost/token/run detail
-- is never stored, so no later bug or query can expose it through the link.
CREATE TABLE client_reports (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    org_id             uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    created_by_user_id uuid REFERENCES users(id),
    title              varchar(255) NOT NULL,
    date_from          date NOT NULL,
    date_to            date NOT NULL,
    -- The IANA zone the window's day boundaries were computed in.
    timezone           varchar(64) NOT NULL,
    visible_sections   jsonb NOT NULL DEFAULT '{"cost": false, "tokens": false, "runs": false}'::jsonb,
    snapshot           jsonb NOT NULL,
    share_token        varchar(64) NOT NULL UNIQUE,
    expires_at         timestamptz NOT NULL,
    view_count         integer NOT NULL DEFAULT 0,
    last_viewed_at     timestamptz,
    is_revoked         boolean NOT NULL DEFAULT false,
    CHECK (date_from <= date_to)
);
CREATE INDEX idx_client_reports_org_id ON client_reports(org_id);
CREATE TRIGGER trg_client_reports_updated_at BEFORE UPDATE ON client_reports
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE client_reports ENABLE ROW LEVEL SECURITY;
ALTER TABLE client_reports FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON client_reports
    USING (org_id = current_org_id())
    WITH CHECK (org_id = current_org_id());

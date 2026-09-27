-- Workflow 22 (SOP Library). A SOP playbook belongs to a practice and bundles
-- one agent config + an optional workflow config + declared parameters
-- ({{key}} placeholders with defaults). Every edit writes a new immutable
-- version; a deployment copies one version into a client workspace as
-- ordinary agents/workflows rows.
CREATE TABLE sop_playbooks (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    practice_id     uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name            varchar(255) NOT NULL,
    description     text,
    category        varchar(50),
    current_version integer NOT NULL DEFAULT 1,
    agent_config    jsonb NOT NULL,
    workflow_config jsonb,
    parameters      jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_by      uuid REFERENCES users(id),
    -- Removing a SOP from the library only detaches it: deployed
    -- agents/workflows in client workspaces keep running untouched.
    is_active       boolean NOT NULL DEFAULT true
);
CREATE UNIQUE INDEX sop_playbooks_unique_practice_name_active
    ON sop_playbooks(practice_id, name) WHERE is_active = true;
CREATE TRIGGER trg_sop_playbooks_updated_at BEFORE UPDATE ON sop_playbooks
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE sop_playbook_versions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    sop_playbook_id uuid NOT NULL REFERENCES sop_playbooks(id) ON DELETE CASCADE,
    version         integer NOT NULL,
    agent_config    jsonb NOT NULL,
    workflow_config jsonb,
    parameters      jsonb NOT NULL DEFAULT '[]'::jsonb,
    changelog       text,
    created_by      uuid REFERENCES users(id),
    UNIQUE (sop_playbook_id, version)
);

CREATE TABLE sop_deployments (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    sop_playbook_id     uuid NOT NULL REFERENCES sop_playbooks(id) ON DELETE CASCADE,
    -- Snapshot, refreshed on every sync: the client workspace can label its
    -- managed agent "SOP <name>" without read access to the practice's rows.
    sop_name            varchar(255) NOT NULL,
    deployed_version    integer NOT NULL,
    target_org_id       uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    agent_id            uuid NOT NULL REFERENCES agents(id),
    workflow_id         uuid REFERENCES workflows(id),
    parameter_overrides jsonb NOT NULL DEFAULT '{}'::jsonb,
    deployed_by         uuid REFERENCES users(id),
    deployed_at         timestamptz NOT NULL DEFAULT now(),
    synced_at           timestamptz NOT NULL DEFAULT now(),
    is_active           boolean NOT NULL DEFAULT true
);
-- One live deployment of a given SOP per client workspace.
CREATE UNIQUE INDEX sop_deployments_unique_active
    ON sop_deployments(sop_playbook_id, target_org_id) WHERE is_active = true;
CREATE INDEX idx_sop_deployments_target_org ON sop_deployments(target_org_id);
CREATE INDEX idx_sop_deployments_agent ON sop_deployments(agent_id);
CREATE TRIGGER trg_sop_deployments_updated_at BEFORE UPDATE ON sop_deployments
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE sop_playbooks ENABLE ROW LEVEL SECURITY;
ALTER TABLE sop_playbooks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sop_playbooks
    USING (practice_id = current_org_id())
    WITH CHECK (practice_id = current_org_id());

ALTER TABLE sop_playbook_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sop_playbook_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sop_playbook_versions
    USING (EXISTS (SELECT 1 FROM sop_playbooks p WHERE p.id = sop_playbook_versions.sop_playbook_id AND p.practice_id = current_org_id()))
    WITH CHECK (EXISTS (SELECT 1 FROM sop_playbooks p WHERE p.id = sop_playbook_versions.sop_playbook_id AND p.practice_id = current_org_id()));

-- Visible to the client workspace it lives in (its own "managed by SOP"
-- labels) and to the owning practice; writable only from the client side,
-- since deploy/sync write the deployment in the same transaction as the
-- client's agents/workflows rows.
ALTER TABLE sop_deployments ENABLE ROW LEVEL SECURITY;
ALTER TABLE sop_deployments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sop_deployments
    USING (
        target_org_id = current_org_id()
        OR EXISTS (SELECT 1 FROM sop_playbooks p WHERE p.id = sop_deployments.sop_playbook_id AND p.practice_id = current_org_id())
    )
    WITH CHECK (target_org_id = current_org_id());

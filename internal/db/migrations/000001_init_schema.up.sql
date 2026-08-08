
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- organizations ---------------------------------------------------------
CREATE TABLE organizations (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    name                  varchar(255) NOT NULL,
    slug                  varchar(100) NOT NULL UNIQUE,
    clerk_org_id          varchar(255) NOT NULL UNIQUE,
    active_api_key_id     uuid,
    llm_provider          varchar(50) DEFAULT 'anthropic',
    plan_tier             varchar(50) DEFAULT 'starter',
    max_agents            integer DEFAULT 3,
    max_workflows         integer DEFAULT 5,
    max_rag_storage_gb    integer DEFAULT 5,
    max_mcp_integrations  integer DEFAULT 3,
    stripe_customer_id    varchar(255),
    stripe_subscription_id varchar(255),
    subscription_status   varchar(50) DEFAULT 'trialing',
    trial_ends_at         timestamptz,
    settings              jsonb DEFAULT '{}'::jsonb,
    onboarding_completed  boolean DEFAULT false,
    is_active             boolean DEFAULT true
);
CREATE TRIGGER trg_organizations_updated_at BEFORE UPDATE ON organizations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- users -------------------------------------------------------------------
CREATE TABLE users (
    id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    org_id                   uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    clerk_user_id            varchar(255) NOT NULL UNIQUE,
    email                    varchar(320) NOT NULL,
    full_name                varchar(255),
    avatar_url               text,
    role                     varchar(50) NOT NULL DEFAULT 'member',
    can_manage_api_keys      boolean DEFAULT false,
    can_manage_integrations  boolean DEFAULT false,
    can_approve_workflows    boolean DEFAULT false,
    is_active                boolean DEFAULT true,
    last_login_at            timestamptz
);
CREATE INDEX idx_users_org_id ON users(org_id);
CREATE TRIGGER trg_users_updated_at BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- sessions ------------------------------------------------------------------
CREATE TABLE sessions (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    user_id            uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    org_id             uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    clerk_session_id   varchar(255) UNIQUE,
    ip_address         inet,
    user_agent         text,
    expires_at         timestamptz NOT NULL
);
CREATE INDEX idx_sessions_org_id ON sessions(org_id);
CREATE INDEX idx_sessions_user_id ON sessions(user_id);
CREATE TRIGGER trg_sessions_updated_at BEFORE UPDATE ON sessions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- api_key_registry ----------------------------------------------------------
CREATE TABLE api_key_registry (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    org_id         uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    provider       varchar(50) NOT NULL,
    key_prefix     varchar(20) NOT NULL,
    encrypted_key  varchar(500) NOT NULL,
    kms_key_id     varchar(255) NOT NULL,
    is_valid       boolean DEFAULT true,
    last_used_at   timestamptz
);
CREATE INDEX idx_api_key_registry_org_id ON api_key_registry(org_id);
CREATE TRIGGER trg_api_key_registry_updated_at BEFORE UPDATE ON api_key_registry
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- mcp_connections -------------------------------------------------------------
CREATE TABLE mcp_connections (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    org_id                 uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    service_name           varchar(100) NOT NULL,
    display_name           varchar(255),
    credential_provider    varchar(50) DEFAULT 'manual',
    encrypted_credentials  text,
    oauth_status           varchar(50) DEFAULT 'pending',
    oauth_scopes           jsonb DEFAULT '[]'::jsonb,
    token_expires_at       timestamptz,
    is_active              boolean DEFAULT true,
    last_used_at           timestamptz
);
CREATE INDEX idx_mcp_connections_org_id ON mcp_connections(org_id);
CREATE INDEX idx_mcp_connections_org_service ON mcp_connections(org_id, service_name);
CREATE TRIGGER trg_mcp_connections_updated_at BEFORE UPDATE ON mcp_connections
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- agents ----------------------------------------------------------------------
CREATE TABLE agents (
    id                          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at                  timestamptz NOT NULL DEFAULT now(),
    updated_at                  timestamptz NOT NULL DEFAULT now(),
    org_id                      uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name                        varchar(255) NOT NULL,
    slug                        varchar(100) NOT NULL,
    description                 text,
    agent_type                  varchar(50) NOT NULL DEFAULT 'specialist',
    model                       varchar(100) DEFAULT 'claude-3-7-sonnet-20250219',
    system_prompt               text NOT NULL,
    context_window_tokens       integer DEFAULT 200000,
    max_output_tokens           integer DEFAULT 4096,
    temperature                 double precision DEFAULT 0.3,
    a2a_endpoint                text,
    team_role                   varchar(50),
    a2a_manifest                jsonb,
    extended_thinking           boolean DEFAULT false,
    extended_thinking_config    jsonb DEFAULT '{}'::jsonb,
    policy_scope                jsonb DEFAULT '{}'::jsonb,
    allowed_mcp_servers         jsonb DEFAULT '[]'::jsonb,
    is_active                   boolean DEFAULT true,
    version                     integer DEFAULT 1,
    created_by                  uuid REFERENCES users(id)
);
CREATE INDEX idx_agents_org_id ON agents(org_id);
CREATE TRIGGER trg_agents_updated_at BEFORE UPDATE ON agents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- agent_teams -------------------------------------------------------------------
CREATE TABLE agent_teams (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    org_id                  uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name                    varchar(255) NOT NULL,
    description             text,
    orchestrator_agent_id   uuid REFERENCES agents(id),
    a2a_manifest            jsonb,
    max_agent_hops          integer DEFAULT 10,
    parallel_execution      boolean DEFAULT true,
    timeout_seconds         integer DEFAULT 600,
    is_active               boolean DEFAULT true
);
CREATE INDEX idx_agent_teams_org_id ON agent_teams(org_id);
CREATE TRIGGER trg_agent_teams_updated_at BEFORE UPDATE ON agent_teams
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- agent_team_members ----------------------------------------------------------
CREATE TABLE agent_team_members (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    team_id      uuid REFERENCES agent_teams(id) ON DELETE CASCADE,
    agent_id     uuid REFERENCES agents(id) ON DELETE CASCADE,
    role         varchar(50) NOT NULL,
    priority     integer DEFAULT 0
);
CREATE INDEX idx_agent_team_members_team_id ON agent_team_members(team_id);
CREATE INDEX idx_agent_team_members_agent_id ON agent_team_members(agent_id);
CREATE TRIGGER trg_agent_team_members_updated_at BEFORE UPDATE ON agent_team_members
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- workflows ---------------------------------------------------------------------
CREATE TABLE workflows (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    org_id                uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    agent_id              uuid NOT NULL REFERENCES agents(id),
    team_id               uuid REFERENCES agent_teams(id),
    name                  varchar(255) NOT NULL,
    description           text,
    trigger_type          varchar(50) NOT NULL,
    graph_definition      jsonb NOT NULL,
    input_schema          jsonb,
    output_schema         jsonb,
    max_agent_hops        integer DEFAULT 10,
    a2a_enabled           boolean DEFAULT false,
    requires_approval     boolean DEFAULT false,
    approval_conditions   jsonb DEFAULT '[]'::jsonb,
    cron_expression       varchar(100),
    timezone              varchar(50) DEFAULT 'UTC',
    next_run_at           timestamptz,
    is_active             boolean DEFAULT true,
    version               integer DEFAULT 1,
    created_by            uuid REFERENCES users(id)
);
CREATE INDEX idx_workflows_org_id ON workflows(org_id);
CREATE INDEX idx_workflows_agent_id ON workflows(agent_id);
-- Backs the workflow-9 background scheduler's poll query
-- ("next_run_at <= NOW() AND is_active").
CREATE INDEX idx_workflows_next_run_at ON workflows(next_run_at) WHERE is_active;
CREATE TRIGGER trg_workflows_updated_at BEFORE UPDATE ON workflows
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- workflow_runs -------------------------------------------------------------------
CREATE TABLE workflow_runs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    workflow_id   uuid NOT NULL REFERENCES workflows(id),
    org_id        uuid NOT NULL REFERENCES organizations(id),
    triggered_by  uuid REFERENCES users(id),
    status        varchar(50) NOT NULL DEFAULT 'pending',
    run_trace     jsonb
);
CREATE INDEX idx_workflow_runs_org_id ON workflow_runs(org_id);
CREATE INDEX idx_workflow_runs_workflow_id ON workflow_runs(workflow_id);
CREATE INDEX idx_workflow_runs_status ON workflow_runs(status);
CREATE TRIGGER trg_workflow_runs_updated_at BEFORE UPDATE ON workflow_runs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- workflow_steps --------------------------------------------------------------------
CREATE TABLE workflow_steps (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    run_id        uuid NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    node_name     varchar(255) NOT NULL,
    step_type     varchar(50) NOT NULL,
    input_data    jsonb,
    output_data   jsonb,
    duration_ms   integer,
    status        varchar(50) DEFAULT 'completed'
);
CREATE INDEX idx_workflow_steps_run_id ON workflow_steps(run_id);
CREATE TRIGGER trg_workflow_steps_updated_at BEFORE UPDATE ON workflow_steps
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- approvals -----------------------------------------------------------------------
CREATE TABLE approvals (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    run_id        uuid NOT NULL REFERENCES workflow_runs(id),
    org_id        uuid NOT NULL REFERENCES organizations(id),
    status        varchar(50) DEFAULT 'pending',
    context_data  jsonb
);
CREATE INDEX idx_approvals_run_id ON approvals(run_id);
CREATE INDEX idx_approvals_org_id ON approvals(org_id);
CREATE INDEX idx_approvals_pending ON approvals(status) WHERE status = 'pending';
CREATE TRIGGER trg_approvals_updated_at BEFORE UPDATE ON approvals
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- approval_decisions ----------------------------------------------------------------
CREATE TABLE approval_decisions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    approval_id   uuid NOT NULL REFERENCES approvals(id) ON DELETE CASCADE,
    user_id       uuid REFERENCES users(id),
    decision      varchar(50) NOT NULL,
    reason        text
);
CREATE INDEX idx_approval_decisions_approval_id ON approval_decisions(approval_id);
CREATE TRIGGER trg_approval_decisions_updated_at BEFORE UPDATE ON approval_decisions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- documents -----------------------------------------------------------------------------
CREATE TABLE documents (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    org_id              uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    filename            varchar(255) NOT NULL,
    s3_path             text NOT NULL,
    mime_type           varchar(100),
    byte_size           integer,
    category            varchar(50) DEFAULT 'general',
    processing_status   varchar(50) DEFAULT 'pending',
    total_chunks        integer DEFAULT 0,
    indexed_at          timestamptz,
    error_detail        text,
    vector_id           text,
    uploaded_by         uuid REFERENCES users(id)
);
CREATE INDEX idx_documents_org_id ON documents(org_id);
CREATE INDEX idx_documents_processing_status ON documents(processing_status);
CREATE TRIGGER trg_documents_updated_at BEFORE UPDATE ON documents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- document_chunks ----------------------------------------------------------------------
CREATE TABLE document_chunks (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    doc_id        uuid NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    chunk_index   integer NOT NULL,
    pinecone_id   varchar(255) NOT NULL
);
CREATE INDEX idx_document_chunks_doc_id ON document_chunks(doc_id);
CREATE TRIGGER trg_document_chunks_updated_at BEFORE UPDATE ON document_chunks
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- vector_namespaces --------------------------------------------------------------------
CREATE TABLE vector_namespaces (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    org_id        uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    namespace     varchar(255) NOT NULL UNIQUE,
    description   text
);
CREATE INDEX idx_vector_namespaces_org_id ON vector_namespaces(org_id);
CREATE TRIGGER trg_vector_namespaces_updated_at BEFORE UPDATE ON vector_namespaces
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- audit_logs -----------------------------------------------------------------------------
CREATE TABLE audit_logs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    org_id          uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    actor_id        uuid,
    actor_type      varchar(50) NOT NULL,
    action          varchar(255) NOT NULL,
    resource_type   varchar(100),
    resource_id     uuid,
    status          varchar(50),
    ip_address      varchar(45),
    metadata_info   jsonb
);
CREATE INDEX idx_audit_logs_org_id ON audit_logs(org_id);
CREATE INDEX idx_audit_logs_created_at ON audit_logs(created_at);
CREATE TRIGGER trg_audit_logs_updated_at BEFORE UPDATE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- cost_ledger -----------------------------------------------------------------------------
CREATE TABLE cost_ledger (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    org_id               uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    run_id               uuid REFERENCES workflow_runs(id),
    agent_id             uuid REFERENCES agents(id),
    cost_type            varchar(50) NOT NULL,
    provider             varchar(50),
    model                varchar(100),
    input_tokens         integer DEFAULT 0,
    output_tokens        integer DEFAULT 0,
    cached_tokens        integer DEFAULT 0,
    thinking_tokens      integer DEFAULT 0,
    estimated_cost_usd   double precision NOT NULL DEFAULT 0.0
);
CREATE INDEX idx_cost_ledger_org_id ON cost_ledger(org_id);
CREATE INDEX idx_cost_ledger_run_id ON cost_ledger(run_id);
CREATE TRIGGER trg_cost_ledger_updated_at BEFORE UPDATE ON cost_ledger
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

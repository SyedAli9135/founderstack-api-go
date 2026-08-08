-- Row-Level Security: one policy per table, scoped to the Postgres session
-- variable "app.current_org_id" — the app sets this with SET LOCAL at the
-- start of every request transaction (see CLAUDE.md § No ORM). Policies are
-- deny-by-default: current_org_id() returns NULL when the session variable
-- is unset, and "org_id = NULL" is never true in SQL, so a connection that
-- forgets to SET LOCAL sees zero rows rather than every row.
--
-- Applies to app_user, a new non-superuser role created below — RLS does
-- NOT apply to superusers or table owners, so this is a genuine no-op for
-- the "postgres" role the migration runner (and today's app) connects as.
-- Wiring the API's runtime connection pool to actually use app_user (plus a
-- second, BYPASSRLS role for system contexts like the Clerk webhook, which
-- legitimately writes orgs it has never seen before) is workflow-2+ work,
-- not done here — see CLAUDE.md.

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = 'app_user') THEN
        -- Local-dev-only credential, intentionally plaintext here — same
        -- posture as POSTGRES_PASSWORD in docker-compose.local.yml. A real
        -- deployment provisions this role's password via Secrets Manager,
        -- never via a versioned migration.
        CREATE ROLE app_user WITH LOGIN PASSWORD 'app_password' NOSUPERUSER NOCREATEDB NOCREATEROLE;
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO app_user;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_user;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_user;

-- audit_logs is append-only by design (see its doc comment in
-- app/models/observability.py) — enforce that at the DB layer, not just by
-- app-level discipline.
REVOKE UPDATE, DELETE ON audit_logs FROM app_user;

CREATE FUNCTION current_org_id() RETURNS uuid AS $$
    -- NULLIF guards the case where the session var was SET LOCAL to '' —
    -- casting an empty string to uuid raises, NULLIF turns it into a clean
    -- NULL first so an unset-or-blank var behaves identically.
    SELECT NULLIF(current_setting('app.current_org_id', true), '')::uuid;
$$ LANGUAGE sql STABLE;

-- organizations is the tenant root: scoped by its own id, not an org_id column.
ALTER TABLE organizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE organizations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON organizations
    USING (id = current_org_id())
    WITH CHECK (id = current_org_id());

-- Tables with a direct org_id column.
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'users', 'sessions', 'api_key_registry', 'mcp_connections',
        'agents', 'agent_teams', 'workflows', 'workflow_runs',
        'approvals', 'documents', 'vector_namespaces', 'audit_logs',
        'cost_ledger'
    ]
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format(
            'CREATE POLICY tenant_isolation ON %I USING (org_id = current_org_id()) WITH CHECK (org_id = current_org_id())',
            t
        );
    END LOOP;
END
$$;

-- Tables with no org_id column of their own — scoped transitively through
-- their parent. agent_team_members has two nullable parents (team_id,
-- agent_id per the source model), so it's scoped via either.

ALTER TABLE agent_team_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_team_members FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON agent_team_members
    USING (
        EXISTS (SELECT 1 FROM agent_teams t WHERE t.id = agent_team_members.team_id AND t.org_id = current_org_id())
        OR EXISTS (SELECT 1 FROM agents a WHERE a.id = agent_team_members.agent_id AND a.org_id = current_org_id())
    )
    WITH CHECK (
        EXISTS (SELECT 1 FROM agent_teams t WHERE t.id = agent_team_members.team_id AND t.org_id = current_org_id())
        OR EXISTS (SELECT 1 FROM agents a WHERE a.id = agent_team_members.agent_id AND a.org_id = current_org_id())
    );

ALTER TABLE workflow_steps ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_steps FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON workflow_steps
    USING (EXISTS (SELECT 1 FROM workflow_runs r WHERE r.id = workflow_steps.run_id AND r.org_id = current_org_id()))
    WITH CHECK (EXISTS (SELECT 1 FROM workflow_runs r WHERE r.id = workflow_steps.run_id AND r.org_id = current_org_id()));

ALTER TABLE approval_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE approval_decisions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON approval_decisions
    USING (EXISTS (SELECT 1 FROM approvals ap WHERE ap.id = approval_decisions.approval_id AND ap.org_id = current_org_id()))
    WITH CHECK (EXISTS (SELECT 1 FROM approvals ap WHERE ap.id = approval_decisions.approval_id AND ap.org_id = current_org_id()));

ALTER TABLE document_chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE document_chunks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON document_chunks
    USING (EXISTS (SELECT 1 FROM documents d WHERE d.id = document_chunks.doc_id AND d.org_id = current_org_id()))
    WITH CHECK (EXISTS (SELECT 1 FROM documents d WHERE d.id = document_chunks.doc_id AND d.org_id = current_org_id()));

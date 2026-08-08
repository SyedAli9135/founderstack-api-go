DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'organizations', 'users', 'sessions', 'api_key_registry', 'mcp_connections',
        'agents', 'agent_teams', 'agent_team_members', 'workflows', 'workflow_runs',
        'workflow_steps', 'approvals', 'approval_decisions', 'documents',
        'document_chunks', 'vector_namespaces', 'audit_logs', 'cost_ledger'
    ]
    LOOP
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', t);
        EXECUTE format('ALTER TABLE %I NO FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I DISABLE ROW LEVEL SECURITY', t);
    END LOOP;
END
$$;

DROP FUNCTION IF EXISTS current_org_id();

REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM app_user;
REVOKE USAGE ON SCHEMA public FROM app_user;
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM app_user;
DROP OWNED BY app_user;
DROP ROLE IF EXISTS app_user;

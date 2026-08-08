-- A second, privileged role for contexts that legitimately need to see
-- across every tenant: the Clerk webhook handler (workflow 2), which
-- creates organizations it has never seen before and so has no org_id to
-- scope by yet, and the workflow-9 background scheduler, which polls
-- next_run_at across every org. Deliberately NOT reused for app_user's
-- traffic — a webhook that forgets to scope a query should fail loud, not
-- silently see nothing (app_user's fail-closed default) or silently see
-- everything (if app_user itself bypassed RLS).
--
-- No Go code connects through this role yet — nothing in this codebase is
-- a webhook or scheduler yet. It's created here, ready, the same way the
-- Pinecone indexes existed before any Go code queried them.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = 'app_system') THEN
        -- Local-dev-only credential — see 000002's app_user for the same note.
        CREATE ROLE app_system WITH LOGIN PASSWORD 'app_system_password'
            NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS;
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO app_system;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_system;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_system;

-- Append-only, same as for app_user.
REVOKE UPDATE, DELETE ON audit_logs FROM app_system;

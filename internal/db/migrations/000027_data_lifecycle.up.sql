-- Deleting data the app roles can't delete themselves. audit_logs is
-- append-only for app_user/app_system by design, and workflow_runs/approvals
-- reference organizations without a cascade; both stop a plain DELETE. These
-- functions run as the table owner and are executable by app_system only.

CREATE OR REPLACE FUNCTION purge_organization(p_org uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    -- A guard against a buggy caller, not authorization: only a deactivated
    -- workspace with no workspaces of its own may be purged.
    IF NOT EXISTS (SELECT 1 FROM organizations WHERE id = p_org AND is_active = false) THEN
        RAISE EXCEPTION 'organization % is not deactivated', p_org;
    END IF;
    IF EXISTS (SELECT 1 FROM organizations WHERE parent_practice_id = p_org) THEN
        RAISE EXCEPTION 'organization % still has client workspaces', p_org;
    END IF;

    DELETE FROM cost_ledger WHERE org_id = p_org;
    DELETE FROM approval_decisions WHERE approval_id IN (SELECT id FROM approvals WHERE org_id = p_org);
    DELETE FROM approvals WHERE org_id = p_org;
    -- sub-runs cascade from their parent; steps cascade from the run.
    DELETE FROM workflow_runs WHERE org_id = p_org;
    -- audit_logs, users, agents, documents, ... cascade from the organization.
    DELETE FROM organizations WHERE id = p_org;
END $$;

-- One bounded batch per call (the caller loops until every count is zero), so
-- no single statement outruns app_system's statement_timeout.
CREATE OR REPLACE FUNCTION purge_expired_records(
    p_audit_days int, p_cost_days int, p_steps_days int, p_reports_grace_days int,
    p_stripe_events_days int, p_batch int,
    OUT audit_logs_deleted int, OUT cost_ledger_deleted int, OUT workflow_steps_deleted int,
    OUT client_reports_deleted int, OUT stripe_events_deleted int)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    WITH d AS (DELETE FROM audit_logs WHERE id IN
        (SELECT id FROM audit_logs WHERE created_at < now() - make_interval(days => p_audit_days) LIMIT p_batch) RETURNING 1)
    SELECT count(*) INTO audit_logs_deleted FROM d;
    WITH d AS (DELETE FROM cost_ledger WHERE id IN
        (SELECT id FROM cost_ledger WHERE created_at < now() - make_interval(days => p_cost_days) LIMIT p_batch) RETURNING 1)
    SELECT count(*) INTO cost_ledger_deleted FROM d;
    WITH d AS (DELETE FROM workflow_steps WHERE id IN
        (SELECT id FROM workflow_steps WHERE created_at < now() - make_interval(days => p_steps_days) LIMIT p_batch) RETURNING 1)
    SELECT count(*) INTO workflow_steps_deleted FROM d;
    WITH d AS (DELETE FROM client_reports WHERE id IN
        (SELECT id FROM client_reports WHERE expires_at < now() - make_interval(days => p_reports_grace_days) LIMIT p_batch) RETURNING 1)
    SELECT count(*) INTO client_reports_deleted FROM d;
    WITH d AS (DELETE FROM stripe_events WHERE id IN
        (SELECT id FROM stripe_events WHERE received_at < now() - make_interval(days => p_stripe_events_days) LIMIT p_batch) RETURNING 1)
    SELECT count(*) INTO stripe_events_deleted FROM d;
END $$;

REVOKE ALL ON FUNCTION purge_organization(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION purge_expired_records(int, int, int, int, int, int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION purge_organization(uuid) TO app_system;
GRANT EXECUTE ON FUNCTION purge_expired_records(int, int, int, int, int, int) TO app_system;

-- Workflow 10: approvals gain a real expiry (24h auto-expiry acceptance
-- criterion), organizations get a single configured Slack channel for
-- approval notifications (not a per-user DM — see WORKFLOW_PLAN_GO.md's
-- scope note), and a new push_subscriptions table stores Web Push
-- subscriptions per (org, user, browser).

ALTER TABLE approvals ADD COLUMN expires_at timestamptz;
CREATE INDEX idx_approvals_pending_expiry ON approvals (status, expires_at) WHERE status = 'pending';

-- Typed column, not a key inside the unused organizations.settings jsonb —
-- matches 000008's own stated preference for dedicated columns over a
-- second, untyped source of truth.
ALTER TABLE organizations ADD COLUMN approvals_slack_channel_id varchar(255);

CREATE TABLE push_subscriptions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    org_id      uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    endpoint    text NOT NULL,
    p256dh_key  text NOT NULL,
    auth_key    text NOT NULL,
    UNIQUE (org_id, user_id, endpoint)
);
CREATE TRIGGER trg_push_subscriptions_updated_at BEFORE UPDATE ON push_subscriptions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE push_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE push_subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON push_subscriptions
    USING (org_id = current_org_id()) WITH CHECK (org_id = current_org_id());

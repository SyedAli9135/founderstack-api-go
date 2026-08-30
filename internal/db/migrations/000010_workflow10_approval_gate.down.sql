DROP TABLE IF EXISTS push_subscriptions;
ALTER TABLE organizations DROP COLUMN IF EXISTS approvals_slack_channel_id;
DROP INDEX IF EXISTS idx_approvals_pending_expiry;
ALTER TABLE approvals DROP COLUMN IF EXISTS expires_at;

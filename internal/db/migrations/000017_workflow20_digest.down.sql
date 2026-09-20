ALTER TABLE organizations DROP COLUMN IF EXISTS digest_last_sent_at;
ALTER TABLE organizations DROP COLUMN IF EXISTS digest_timezone;
ALTER TABLE organizations DROP COLUMN IF EXISTS digest_send_hour;
ALTER TABLE organizations DROP COLUMN IF EXISTS digest_enabled;

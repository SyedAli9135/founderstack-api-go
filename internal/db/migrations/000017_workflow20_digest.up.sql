-- Workflow 20 (Daily Email Digest). Reuses the Brevo sender already built
-- for workflow 10's approval-gate notifications -- no new email provider
-- or credential needed, same free-forever-tier reasoning.
--
-- digest_last_sent_at is the double-send guard: the scheduler's ticker
-- fires once an hour, so a process restart inside the same hour a digest
-- already went out (or a slightly-late tick) must not resend it. It's
-- compared against "today" in the org's own digest_timezone, not UTC, so
-- a founder in a timezone behind UTC doesn't get skipped a day.
ALTER TABLE organizations ADD COLUMN digest_enabled boolean NOT NULL DEFAULT true;
ALTER TABLE organizations ADD COLUMN digest_send_hour integer NOT NULL DEFAULT 8
    CHECK (digest_send_hour BETWEEN 0 AND 23);
ALTER TABLE organizations ADD COLUMN digest_timezone varchar(64) NOT NULL DEFAULT 'UTC';
ALTER TABLE organizations ADD COLUMN digest_last_sent_at timestamptz;

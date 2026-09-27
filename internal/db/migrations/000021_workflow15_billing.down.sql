DROP TABLE IF EXISTS stripe_events;
DROP INDEX IF EXISTS organizations_stripe_customer_id_key;
ALTER TABLE organizations ALTER COLUMN trial_ends_at DROP DEFAULT;
ALTER TABLE organizations
    DROP COLUMN IF EXISTS cancel_at_period_end,
    DROP COLUMN IF EXISTS current_period_end;

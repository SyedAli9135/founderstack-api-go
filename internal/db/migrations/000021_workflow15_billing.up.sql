-- Workflow 15 (Manage Billing & Subscription). The plan/limit columns
-- already exist on organizations (000001); this adds what Stripe's
-- subscription lifecycle needs on top, plus a processed-event log so a
-- redelivered webhook doesn't repeat its side effects (the payment-failed
-- email).
ALTER TABLE organizations
    ADD COLUMN current_period_end   timestamptz,
    ADD COLUMN cancel_at_period_end boolean NOT NULL DEFAULT false;

-- Every org without a subscription is on a 14-day free trial of Starter.
-- Existing orgs get theirs counted from when they were created.
ALTER TABLE organizations ALTER COLUMN trial_ends_at SET DEFAULT now() + interval '14 days';
UPDATE organizations SET trial_ends_at = created_at + interval '14 days'
    WHERE trial_ends_at IS NULL AND stripe_subscription_id IS NULL;

-- A webhook finds its org by Stripe customer, so one customer maps to at
-- most one org.
CREATE UNIQUE INDEX organizations_stripe_customer_id_key
    ON organizations(stripe_customer_id) WHERE stripe_customer_id IS NOT NULL;

-- Written only by the Stripe webhook through app_system; app_user (the
-- tenant-scoped role) has no business reading it.
CREATE TABLE stripe_events (
    id          varchar(255) PRIMARY KEY,
    type        varchar(100) NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now()
);
REVOKE ALL ON stripe_events FROM app_user;

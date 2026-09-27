-- Workflow 24 (Practice Billing). A practice's plan includes some client
-- workspaces; beyond that, each active one is billed as an extra Stripe
-- subscription item, up to max_client_workspaces. Both columns are kept in
-- step with the plan by the subscription sync (internal/core/billing), the
-- same way max_agents etc. are; the values here are Starter's, matching
-- billing.DefaultPlan.
ALTER TABLE organizations ADD COLUMN included_client_workspaces integer NOT NULL DEFAULT 1;
ALTER TABLE organizations ALTER COLUMN max_client_workspaces SET DEFAULT 1;

UPDATE organizations SET
    included_client_workspaces = CASE plan_tier WHEN 'growth' THEN 3 WHEN 'studio' THEN 10 ELSE 1 END,
    max_client_workspaces      = CASE plan_tier WHEN 'growth' THEN 25 WHEN 'studio' THEN 100 ELSE 1 END;

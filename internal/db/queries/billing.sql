-- name: GetOrgBilling :one
SELECT o.id, o.name, o.organization_type, o.plan_tier, o.subscription_status, o.trial_ends_at,
       o.current_period_end, o.cancel_at_period_end, o.stripe_customer_id, o.stripe_subscription_id,
       o.max_agents, o.max_workflows, o.max_rag_storage_gb, o.max_mcp_integrations,
       o.included_client_workspaces, o.max_client_workspaces,
       (SELECT COUNT(*) FROM organizations c
            WHERE c.parent_practice_id = o.id AND c.is_active = true)::bigint AS active_client_workspaces
FROM organizations o WHERE o.id = $1;

-- name: GetOrgBillingUsage :one
SELECT
    (SELECT COUNT(*) FROM agents a WHERE a.org_id = $1 AND a.is_active = true)::bigint AS agents,
    (SELECT COUNT(*) FROM workflows w WHERE w.org_id = $1 AND w.is_active = true)::bigint AS workflows,
    (SELECT COALESCE(SUM(d.byte_size), 0) FROM documents d
        WHERE d.org_id = $1 AND d.processing_status <> 'deleting')::bigint AS storage_bytes,
    (SELECT COUNT(*) FROM mcp_connections m
        WHERE m.org_id = $1 AND m.is_active = true AND m.oauth_status = 'connected')::bigint AS integrations;

-- name: SetOrgStripeCustomer :exec
UPDATE organizations SET stripe_customer_id = $2 WHERE id = $1;

-- name: GetOrgIDByStripeCustomer :one
SELECT id FROM organizations WHERE stripe_customer_id = $1;

-- name: LockOrgSubscription :one
-- Serializes concurrent webhook deliveries for one org, so "which
-- subscription is current" is decided against the latest row.
SELECT stripe_subscription_id, subscription_status FROM organizations WHERE id = $1 FOR UPDATE;

-- name: ApplySubscriptionState :exec
UPDATE organizations SET
    plan_tier = $2, subscription_status = $3, stripe_subscription_id = $4,
    current_period_end = $5, cancel_at_period_end = $6, trial_ends_at = $7,
    max_agents = $8, max_workflows = $9, max_rag_storage_gb = $10, max_mcp_integrations = $11,
    included_client_workspaces = $12, max_client_workspaces = $13
WHERE id = $1;

-- name: InheritPracticePlan :exec
-- A client workspace is billed through its practice, so it runs on the
-- practice's plan limits. Called whenever the practice's plan changes and
-- when a workspace is created under it.
UPDATE organizations c SET
    plan_tier = p.plan_tier, max_agents = p.max_agents, max_workflows = p.max_workflows,
    max_rag_storage_gb = p.max_rag_storage_gb, max_mcp_integrations = p.max_mcp_integrations
FROM organizations p
WHERE p.id = sqlc.arg(practice_id) AND c.parent_practice_id = p.id
  AND (sqlc.narg(workspace_id)::uuid IS NULL OR c.id = sqlc.narg(workspace_id)::uuid);

-- name: ListPracticesWithLiveSubscriptions :many
SELECT id FROM organizations
WHERE organization_type = 'practice' AND is_active = true
  AND stripe_subscription_id IS NOT NULL
  AND subscription_status IN ('active', 'trialing', 'past_due');

-- name: RecordStripeEvent :one
-- Returns no row when the event was already processed (a redelivery).
INSERT INTO stripe_events (id, type) VALUES ($1, $2)
ON CONFLICT (id) DO NOTHING
RETURNING id;

-- name: ListOrgBillingContacts :many
SELECT email, full_name FROM users
WHERE org_id = $1 AND is_active = true AND role IN ('owner', 'admin');

-- name: CountActiveWorkflows :one
SELECT COUNT(*) FROM workflows WHERE org_id = $1 AND is_active = true;

-- name: GetOrganizationMaxWorkflows :one
SELECT max_workflows FROM organizations WHERE id = $1;

-- name: GetOrgStorageAllowance :one
SELECT
    o.max_rag_storage_gb,
    (SELECT COALESCE(SUM(d.byte_size), 0) FROM documents d
        WHERE d.org_id = o.id AND d.processing_status <> 'deleting')::bigint AS used_bytes
FROM organizations o WHERE o.id = $1;

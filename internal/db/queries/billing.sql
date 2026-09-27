-- name: GetOrgBilling :one
SELECT id, name, organization_type, plan_tier, subscription_status, trial_ends_at,
       current_period_end, cancel_at_period_end, stripe_customer_id, stripe_subscription_id,
       max_agents, max_workflows, max_rag_storage_gb, max_mcp_integrations
FROM organizations WHERE id = $1;

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
    max_agents = $8, max_workflows = $9, max_rag_storage_gb = $10, max_mcp_integrations = $11
WHERE id = $1;

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

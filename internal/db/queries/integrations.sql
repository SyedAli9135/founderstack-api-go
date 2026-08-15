-- Queries backing third-party integration connections (workflow 4),
-- against the mcp_connections table. Per-org reads/writes (connect,
-- callback, api-key, status, delete) run through app_user via
-- tenant.WithTx, same as api_key_registry in api_keys.sql. The two
-- "System" queries below are the exception — the background token-refresh
-- job scans expiring connections across every org, which is inherently a
-- cross-tenant system-context operation (same reasoning as clerk_sync.sql),
-- so those run against app_system (BYPASSRLS) directly, never through
-- tenant.WithTx.

-- name: UpsertConnection :one
INSERT INTO mcp_connections (
    org_id, service_name, display_name, credential_provider,
    encrypted_credentials, oauth_status, oauth_scopes, token_expires_at, is_active
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true)
ON CONFLICT (org_id, service_name) DO UPDATE SET
    display_name          = EXCLUDED.display_name,
    credential_provider    = EXCLUDED.credential_provider,
    encrypted_credentials  = EXCLUDED.encrypted_credentials,
    oauth_status           = EXCLUDED.oauth_status,
    oauth_scopes           = EXCLUDED.oauth_scopes,
    token_expires_at       = EXCLUDED.token_expires_at,
    is_active               = true
RETURNING id;

-- name: GetConnectionByOrgService :one
SELECT id, service_name, display_name, credential_provider, encrypted_credentials,
       oauth_status, oauth_scopes, token_expires_at, is_active, created_at
FROM mcp_connections
WHERE org_id = $1 AND service_name = $2;

-- name: ListConnectionsByOrg :many
SELECT service_name, oauth_status, oauth_scopes, is_active, created_at
FROM mcp_connections
WHERE org_id = $1;

-- name: RevokeConnection :execrows
UPDATE mcp_connections
SET is_active = false, oauth_status = 'revoked'
WHERE org_id = $1 AND service_name = $2;

-- name: MarkConnectionExpired :execrows
UPDATE mcp_connections
SET oauth_status = 'expired'
WHERE org_id = $1 AND service_name = $2;

-- name: UpdateConnectionTokens :execrows
UPDATE mcp_connections
SET encrypted_credentials = $3, token_expires_at = $4, oauth_status = 'connected'
WHERE org_id = $1 AND service_name = $2;

-- name: ListExpiringConnectionsSystem :many
-- Used only by the background refresh job (app_system pool). Scoped to
-- oauth_status = 'connected' so a already-expired or revoked connection
-- isn't retried every 30 minutes forever.
SELECT id, org_id, service_name, encrypted_credentials
FROM mcp_connections
WHERE is_active = true
  AND oauth_status = 'connected'
  AND token_expires_at IS NOT NULL
  AND token_expires_at < $1;

-- name: UpdateConnectionTokensByIDSystem :execrows
-- Used only by the background refresh job (app_system pool) — targets a
-- specific connection by id, already scoped to the right org by virtue of
-- having come from ListExpiringConnectionsSystem's own row.
UPDATE mcp_connections
SET encrypted_credentials = $2, token_expires_at = $3, oauth_status = 'connected'
WHERE id = $1;

-- name: MarkConnectionExpiredByIDSystem :execrows
UPDATE mcp_connections
SET oauth_status = 'expired'
WHERE id = $1;

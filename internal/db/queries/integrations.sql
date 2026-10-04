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
-- oauth_status = 'connected' so an already-expired or revoked connection
-- isn't retried every tick forever, and to active orgs so a deleted
-- workspace's tokens aren't kept alive.
SELECT c.id, c.org_id, c.service_name, c.encrypted_credentials
FROM mcp_connections c
JOIN organizations o ON o.id = c.org_id AND o.is_active = true
WHERE c.is_active = true
  AND c.oauth_status = 'connected'
  AND c.token_expires_at IS NOT NULL
  AND c.token_expires_at < $1;

-- name: LockConnectionForRefreshSystem :one
-- Claims one expiring connection for this caller: SKIP LOCKED means another
-- instance already refreshing it (or one that finished and pushed the expiry
-- out of the window) yields no row. Must run inside the transaction that does
-- the refresh — the lock lasts until it ends.
SELECT id, org_id, service_name, encrypted_credentials
FROM mcp_connections
WHERE id = $1
  AND is_active = true
  AND oauth_status = 'connected'
  AND token_expires_at IS NOT NULL
  AND token_expires_at < $2
FOR UPDATE SKIP LOCKED;

-- name: GetConnectionByOrgServiceForUpdate :one
-- The on-demand refresh path: concurrent tool calls needing the same expired
-- token queue on this lock, and the ones after the first find it fresh.
SELECT id, service_name, encrypted_credentials, oauth_scopes, oauth_status, is_active
FROM mcp_connections
WHERE org_id = $1 AND service_name = $2
FOR UPDATE;

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

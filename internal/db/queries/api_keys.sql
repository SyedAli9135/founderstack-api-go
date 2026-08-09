-- Queries backing BYOK API key management (workflow 3). Run through
-- app_user via tenant.WithTx — this is genuinely tenant-scoped data, not a
-- system-context lookup like clerk_sync.sql or auth.sql.

-- name: GetActiveAnthropicKeyByOrgID :one
SELECT id, encrypted_key, key_prefix, is_valid
FROM api_key_registry
WHERE org_id = $1 AND provider = 'anthropic' AND is_valid = true;

-- name: UpsertAnthropicKey :one
INSERT INTO api_key_registry (org_id, provider, key_prefix, encrypted_key, kms_key_id, is_valid)
VALUES ($1, 'anthropic', $2, $3, $4, true)
ON CONFLICT (org_id, provider) DO UPDATE SET
    key_prefix = EXCLUDED.key_prefix,
    encrypted_key = EXCLUDED.encrypted_key,
    kms_key_id = EXCLUDED.kms_key_id,
    is_valid = true
RETURNING id;

-- name: SetOrganizationActiveApiKey :exec
UPDATE organizations SET active_api_key_id = $2, onboarding_completed = true WHERE id = $1;

-- name: GetAnthropicKeyStatus :one
SELECT provider, is_valid, key_prefix, updated_at, last_used_at
FROM api_key_registry
WHERE org_id = $1 AND provider = 'anthropic';

-- name: DeactivateAnthropicKey :execrows
UPDATE api_key_registry SET is_valid = false WHERE org_id = $1 AND provider = 'anthropic';

-- name: ClearOrganizationActiveApiKey :exec
UPDATE organizations SET active_api_key_id = NULL WHERE id = $1;

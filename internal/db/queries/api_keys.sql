-- Queries backing BYOK API key management (workflow 3). Run through
-- app_user via tenant.WithTx — this is genuinely tenant-scoped data, not a
-- system-context lookup like clerk_sync.sql or auth.sql. Provider is a
-- caller-supplied parameter (llm.ProviderID), not baked into the SQL —
-- generalized 2026-08-21 from an Anthropic-only literal to support all 5
-- catalog providers (see internal/core/llm/catalog.go).

-- name: GetActiveKeyByOrgIDAndProvider :one
SELECT id, encrypted_key, key_prefix, is_valid
FROM api_key_registry
WHERE org_id = $1 AND provider = $2 AND is_valid = true;

-- name: UpsertAPIKey :one
INSERT INTO api_key_registry (org_id, provider, key_prefix, encrypted_key, kms_key_id, is_valid)
VALUES ($1, $2, $3, $4, $5, true)
ON CONFLICT (org_id, provider) DO UPDATE SET
    key_prefix = EXCLUDED.key_prefix,
    encrypted_key = EXCLUDED.encrypted_key,
    kms_key_id = EXCLUDED.kms_key_id,
    is_valid = true
RETURNING id;

-- name: SetOrganizationActiveApiKey :exec
UPDATE organizations SET active_api_key_id = $2, llm_provider = $3, onboarding_completed = true WHERE id = $1;

-- name: GetKeyStatusByProvider :one
SELECT provider, is_valid, key_prefix, updated_at, last_used_at
FROM api_key_registry
WHERE org_id = $1 AND provider = $2;

-- name: ListKeyStatuses :many
-- Every provider the org has ever submitted a key for (valid or not),
-- annotated with whether it's the org's *currently active* provider —
-- backs GET /api/v1/settings/api-key/providers, which merges this with
-- llm.Catalog so the frontend gets one request for "every supported
-- provider, plus this org's status for each." COALESCE forces a real
-- false (not SQL NULL) when llm_provider is unset, so the generated Go
-- field is a plain bool, not a nullable pointer.
SELECT
    r.provider,
    r.is_valid,
    r.key_prefix,
    r.updated_at,
    r.last_used_at,
    COALESCE(o.llm_provider = r.provider AND o.active_api_key_id = r.id, false)::bool AS is_active
FROM api_key_registry r
JOIN organizations o ON o.id = r.org_id
WHERE r.org_id = $1
ORDER BY r.provider;

-- name: DeactivateKeyByProvider :execrows
UPDATE api_key_registry SET is_valid = false WHERE org_id = $1 AND provider = $2;

-- name: ClearOrganizationActiveApiKeyForProvider :exec
-- Only clears active_api_key_id/llm_provider when provider is the org's
-- *currently active* provider — with multiple providers now storable per
-- org, deactivating a non-active provider's key must not clobber a
-- different, still-active provider's pointer.
UPDATE organizations SET active_api_key_id = NULL, llm_provider = NULL
WHERE id = $1 AND llm_provider = $2;

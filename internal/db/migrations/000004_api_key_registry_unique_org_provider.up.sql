-- The Python original's app/models/integration.py comment states "we
-- enforce one Anthropic key per org for simplicity in V1", but its actual
-- implementation is an application-level select-then-insert-or-update with
-- no DB constraint backing that invariant — a race between two concurrent
-- requests could create two rows for the same (org_id, provider). This
-- adds the constraint the stated intent already assumed existed, enabling
-- a real INSERT ... ON CONFLICT upsert instead of a racy read-then-write.
ALTER TABLE api_key_registry
    ADD CONSTRAINT api_key_registry_org_provider_unique UNIQUE (org_id, provider);

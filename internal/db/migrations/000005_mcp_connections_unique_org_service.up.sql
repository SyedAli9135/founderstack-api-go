-- mcp_connections has an (org_id, service_name) lookup index
-- (idx_mcp_connections_org_service) but no uniqueness constraint backing
-- "one connection per org per service" — the same gap 000004 closed for
-- api_key_registry. Without this, connecting the same service twice (e.g.
-- a double-click on "Connect Slack", or a retried OAuth callback) would
-- create two rows instead of upserting one. Adds the constraint so
-- SaveConnection can use a real INSERT ... ON CONFLICT upsert.
ALTER TABLE mcp_connections
    ADD CONSTRAINT mcp_connections_org_service_unique UNIQUE (org_id, service_name);

-- 'claude-sonnet-5' isn't a model ID Anthropic serves; the current Sonnet is
-- 'claude-sonnet-5-5'. Installing a template copied the bad ID into the new
-- agent, which would fail on its first real run.
ALTER TABLE agent_templates ALTER COLUMN model SET DEFAULT 'claude-sonnet-5-5';
UPDATE agent_templates SET model = 'claude-sonnet-5-5' WHERE model = 'claude-sonnet-5';

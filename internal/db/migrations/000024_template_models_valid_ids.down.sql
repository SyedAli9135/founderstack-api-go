ALTER TABLE agent_templates ALTER COLUMN model SET DEFAULT 'claude-sonnet-5';
UPDATE agent_templates SET model = 'claude-sonnet-5' WHERE model = 'claude-sonnet-5-5';

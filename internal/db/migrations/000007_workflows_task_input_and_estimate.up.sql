-- Workflow 8's spec calls for a "task input template" textarea (default
-- instructions seeded into each run) and an "estimated_manual_minutes"
-- field (how long this task takes a founder by hand — used by Workflow 11
-- to compute hours saved per run), but neither column ever existed on
-- `workflows` in either backend — the Python original's app/models/ai.py
-- Workflow model has the same gap. Adding both here rather than working
-- around their absence.
ALTER TABLE workflows ADD COLUMN task_input_template text;
ALTER TABLE workflows ADD COLUMN estimated_manual_minutes integer;

-- Workflow 12 (RAG search) needs a real role-based ACL to satisfy its own
-- acceptance criterion ("viewer cannot see owner-only docs") -- no such
-- concept existed before this (documents had no visibility/ACL column at
-- all; isolation was purely tenant-level). Kept deliberately minimal:
-- 2 values, no new role vocabulary -- see WORKFLOW_PLAN_GO.md's Workflow 12
-- scope note for the full reasoning (this is not workflow 13's team/role
-- management, just the document-level flag that feature will eventually
-- build on).
ALTER TABLE documents ADD COLUMN visibility varchar(20) NOT NULL DEFAULT 'all_members';
ALTER TABLE documents ADD CONSTRAINT documents_visibility_check
    CHECK (visibility IN ('all_members', 'owner_only'));

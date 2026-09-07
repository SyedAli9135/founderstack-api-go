ALTER TABLE documents DROP CONSTRAINT IF EXISTS documents_visibility_check;
ALTER TABLE documents DROP COLUMN IF EXISTS visibility;

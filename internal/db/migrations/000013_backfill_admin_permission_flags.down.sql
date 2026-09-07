-- Deliberately a no-op: reverting this migration by setting the flags back
-- to false would recreate the exact lockout it exists to prevent. Rolling
-- back this migration should not undo the backfill.
SELECT 1;

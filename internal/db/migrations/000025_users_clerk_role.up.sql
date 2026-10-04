-- The role Clerk last reported for this membership (normalized, e.g. "admin").
-- users.role is the role the app enforces and may differ (a "viewer" has no
-- Clerk equivalent; an API role change may not have reached Clerk). A webhook
-- may only replace users.role when Clerk's own role actually changed —
-- otherwise any unrelated membership event would silently undo an app-side
-- demotion.
ALTER TABLE users ADD COLUMN clerk_role varchar(50);
UPDATE users SET clerk_role = CASE WHEN role IN ('owner', 'admin') THEN role ELSE 'member' END;

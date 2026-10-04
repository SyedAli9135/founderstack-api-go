-- Clerk doesn't guarantee webhook order and retries failed deliveries, so a
-- stale membership event can arrive after a newer one. This is the time (from
-- the event's own timestamp) of the newest Clerk event applied to the row;
-- older events are ignored instead of re-activating a removed member or
-- reverting a role.
ALTER TABLE users ADD COLUMN clerk_event_at timestamptz;

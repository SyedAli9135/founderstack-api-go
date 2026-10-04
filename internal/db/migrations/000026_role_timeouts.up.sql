-- Without these, one runaway query or a transaction left open by a crashed
-- request holds a connection (and its locks) indefinitely, and a handful of
-- them exhaust the pool for every tenant. Set on the roles, not in the
-- application, so they hold for every connection path (including poolers that
-- reject startup parameters) and for every tool that logs in as these roles.
--
-- app_system runs the cross-tenant sweeps and holds a transaction open while
-- it calls Stripe from the webhook, so its limits are looser.
ALTER ROLE app_user SET statement_timeout = '30s';
ALTER ROLE app_user SET lock_timeout = '10s';
ALTER ROLE app_user SET idle_in_transaction_session_timeout = '60s';

ALTER ROLE app_system SET statement_timeout = '120s';
ALTER ROLE app_system SET lock_timeout = '10s';
ALTER ROLE app_system SET idle_in_transaction_session_timeout = '120s';

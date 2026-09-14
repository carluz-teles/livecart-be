-- Provider operation logs now use the application logger. Only the historical
-- HTTP log table is removed; webhook queues and business records are retained.
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP TABLE IF EXISTS public.integration_logs RESTRICT;
COMMIT;

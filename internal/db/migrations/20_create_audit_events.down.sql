-- The audit log is lost with the table: export it first, as a regulated
-- exchange must keep it.
DROP TABLE IF EXISTS audit_events;
DROP FUNCTION IF EXISTS audit_events_append_only();

-- Messages that are still pending are lost with the table: roll back only
-- after the relay has published them.
DROP TABLE IF EXISTS outbox_events;

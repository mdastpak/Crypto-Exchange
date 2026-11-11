-- Row seals for the audit log (#337, #139, #51): a keyed MAC over every
-- column of an event, which audit.Record computes with an audit-mac data
-- key (#138), which the database never holds. An event changed with
-- database access alone, after the append-only triggers were turned off, no
-- longer matches its seal.
--
-- Events never change, so they are sealed in their INSERT, at row_version 0,
-- and never by a re-seal. Events written before this migration, or without
-- data keys, have no seal; a seal cannot be added later.
ALTER TABLE audit_events
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER CHECK (seal_key_version > 0),
    ADD CONSTRAINT audit_events_seal_complete CHECK ((row_seal IS NULL) = (seal_key_version IS NULL));

-- audit-mac is a purpose of its own, so that row-mac keeps its rotate,
-- re-seal, and retire cycle: an audit-mac version that seals an event stays
-- readable for good. The server creates its first version at startup.
ALTER TABLE data_keys
    DROP CONSTRAINT data_keys_purpose_check,
    ADD CONSTRAINT data_keys_purpose_check CHECK (purpose IN ('row-mac', 'field-enc', 'blind-index', 'otp-mac', 'audit-mac'));

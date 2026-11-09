-- Row seals (#139): a keyed MAC over the protected columns of a row, so that
-- a change made outside the backend, by someone with access to the database
-- alone, is detected. The MAC key is a row-mac data key (#138), which the
-- database never holds, so nobody can compute a seal in SQL.
--
-- row_version counts the sealed writes of the row; the backend raises it in
-- the statement that writes the seal, and the seal covers it. row_seal and
-- seal_key_version are NULL for a row that was never sealed: rows written
-- before this migration, or by an instance of an earlier release while a
-- release rolls out. internal/rowseal says what happens to them.
ALTER TABLE users
    ADD COLUMN row_version      BIGINT  NOT NULL DEFAULT 0,
    ADD COLUMN row_seal         BYTEA,
    ADD COLUMN seal_key_version INTEGER;
ALTER TABLE user_contacts
    ADD COLUMN row_version      BIGINT  NOT NULL DEFAULT 0,
    ADD COLUMN row_seal         BYTEA,
    ADD COLUMN seal_key_version INTEGER;
ALTER TABLE sessions
    ADD COLUMN row_version      BIGINT  NOT NULL DEFAULT 0,
    ADD COLUMN row_seal         BYTEA,
    ADD COLUMN seal_key_version INTEGER;

-- integrity_findings records the rows whose seal did not match, found when a
-- request read them or by the background verifier. A row has at most one
-- open finding; later detections count on it. Resolving one (a re-seal by
-- an administrator, or a re-seal in report mode) closes it, and a later
-- mismatch opens a new one. The flag is a report: the seal is the source of
-- truth, since whoever can change a row can also change this table.
CREATE TABLE integrity_findings (
    id           BIGSERIAL PRIMARY KEY,
    table_name   TEXT        NOT NULL CHECK (table_name IN ('users', 'user_contacts', 'sessions')),
    row_key      TEXT        NOT NULL,
    -- check is what failed: missing (no seal), mismatch (a seal that does
    -- not match the row), or unknown_key (a seal key version that no
    -- instance holds).
    check_failed TEXT        NOT NULL CHECK (check_failed IN ('missing', 'mismatch', 'unknown_key')),
    detected_by  TEXT        NOT NULL CHECK (detected_by IN ('read', 'verifier')),
    detected_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    seen_count   BIGINT      NOT NULL DEFAULT 1,
    resolved_at  TIMESTAMPTZ,
    -- resolution is resealed (by an administrator, with resolved_by and a
    -- reason), report_mode (re-sealed by the verifier in report mode), or
    -- row_gone (the row no longer exists).
    resolution   TEXT        CHECK (resolution IN ('resealed', 'report_mode', 'row_gone')),
    resolved_by  UUID,
    reason       TEXT        CHECK (length(reason) <= 1000),
    CHECK ((resolved_at IS NULL) = (resolution IS NULL))
);

CREATE UNIQUE INDEX integrity_findings_open ON integrity_findings (table_name, row_key) WHERE resolved_at IS NULL;
CREATE INDEX integrity_findings_newest ON integrity_findings (id DESC);

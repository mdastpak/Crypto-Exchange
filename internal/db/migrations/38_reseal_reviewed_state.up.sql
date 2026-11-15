-- The state of the row that the administrator who asks for a re-seal
-- reviewed (#411). Before, the approval of a second administrator sealed
-- the row as it was at the approval, so a row changed in SQL between the
-- request and the approval was sealed without anybody reviewing it.
--
-- reseal_row_version is the row_version of the row, -1 when it was gone;
-- reseal_row_digest a MAC of its canonical encoding made with version
-- reseal_digest_key_version of the row-mac key, under a tag of its own so
-- that it is never a valid seal of the row, and empty when it was gone;
-- reseal_row_values its values, secret ones masked, as both administrators
-- see them. The approval compares the row with them and refuses when it
-- changed. The columns have no seal of their own, as the other request
-- columns: the approval counts the request only with its sealed audit
-- event, which holds the same values (#337). A request made before this
-- migration has none, and counts as none.
ALTER TABLE integrity_findings
    ADD COLUMN reseal_row_version BIGINT,
    ADD COLUMN reseal_row_digest BYTEA,
    ADD COLUMN reseal_digest_key_version INTEGER,
    ADD COLUMN reseal_row_values JSONB,
    ADD CONSTRAINT integrity_findings_reseal_reviewed_check CHECK (
        (reseal_row_version IS NULL) = (reseal_row_digest IS NULL)
        AND (reseal_row_version IS NULL) = (reseal_digest_key_version IS NULL)
        AND (reseal_row_version IS NULL) = (reseal_row_values IS NULL)
        AND (reseal_row_version IS NULL OR reseal_requested_by IS NOT NULL)
        AND reseal_row_version >= -1
        AND octet_length(reseal_row_digest) IN (0, 32));

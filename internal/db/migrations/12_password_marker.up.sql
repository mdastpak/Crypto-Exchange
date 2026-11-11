-- The record of the account password in the sealed users row (#337). A seal
-- cannot show a row that was deleted, and deleting the user_passwords row of
-- an account dropped the second step of its sign-in. users.password_set_at
-- is set when the account gets its password, and covered by the users seal,
-- so a password row that is gone while the record says it exists is
-- detected.
--
-- The column is left out of the seal while it is NULL, so the seals made
-- before it still match. It is not filled here: a value written outside the
-- backend would not match the seals. The backend records it for an account
-- that has a password the next time the password is proved or changed.
ALTER TABLE users ADD COLUMN password_set_at TIMESTAMPTZ;

ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_check_failed_check,
    ADD CONSTRAINT integrity_findings_check_failed_check
        CHECK (check_failed IN ('missing', 'mismatch', 'unknown_key', 'row_missing'));

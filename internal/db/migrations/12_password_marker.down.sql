-- Findings of missing rows go with the check that names them.
DELETE FROM integrity_findings WHERE check_failed = 'row_missing';
ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_check_failed_check,
    ADD CONSTRAINT integrity_findings_check_failed_check
        CHECK (check_failed IN ('missing', 'mismatch', 'unknown_key'));

-- The seals that cover a recorded password no longer match without it.
ALTER TABLE users DROP COLUMN IF EXISTS password_set_at;

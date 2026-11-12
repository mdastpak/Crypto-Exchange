-- The books are kept for good: the down migration refuses to drop them once
-- an entry exists.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM ledger_entries) THEN
        RAISE EXCEPTION 'the ledger holds entries, which the down migration of 18_ledger would lose';
    END IF;
END $$;

DROP TRIGGER IF EXISTS balance_snapshots_no_truncate ON balance_snapshots;
DROP TRIGGER IF EXISTS balance_snapshots_no_update_or_delete ON balance_snapshots;
DROP TRIGGER IF EXISTS balance_changes_no_truncate ON balance_changes;
DROP TRIGGER IF EXISTS balance_changes_no_update_or_delete ON balance_changes;
DROP TRIGGER IF EXISTS ledger_postings_no_truncate ON ledger_postings;
DROP TRIGGER IF EXISTS ledger_postings_no_update_or_delete ON ledger_postings;
DROP TRIGGER IF EXISTS ledger_entries_no_truncate ON ledger_entries;
DROP TRIGGER IF EXISTS ledger_entries_no_update_or_delete ON ledger_entries;
DROP FUNCTION IF EXISTS ledger_append_only();

DROP TRIGGER IF EXISTS balance_changes_balanced ON balance_changes;
DROP TRIGGER IF EXISTS ledger_postings_balanced ON ledger_postings;
DROP TRIGGER IF EXISTS ledger_entries_balanced ON ledger_entries;
DROP FUNCTION IF EXISTS ledger_entry_balanced();

DROP INDEX IF EXISTS balance_changes_entry;
ALTER TABLE balance_changes
    DROP CONSTRAINT IF EXISTS balance_changes_change_type_check,
    DROP CONSTRAINT IF EXISTS balance_changes_balance_version_unique,
    DROP COLUMN IF EXISTS version,
    DROP COLUMN IF EXISTS entry_id,
    ADD COLUMN reference_id uuid,
    ADD COLUMN reference_type character varying(50);
CREATE INDEX idx_balance_changes_reference ON public.balance_changes USING btree (reference_id, reference_type);

DROP TABLE IF EXISTS ledger_postings;
DROP TABLE IF EXISTS ledger_entries;

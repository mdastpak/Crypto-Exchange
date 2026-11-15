-- The conversions of currency migrations (#215): an entry of kind
-- conversion moves the available funds of a holder in a source currency,
-- through a lock, into the target at the ratio of the migration. Custody
-- must hold the source less and the target more, and the dust under the
-- precision of the target goes to the fees account. The shape of the entry
-- is that of Ledger.Convert (internal/ledger), and nothing else commits.
ALTER TABLE ledger_entries
    DROP CONSTRAINT ledger_entries_kind_check,
    ADD CONSTRAINT ledger_entries_kind_check CHECK (kind IN (
        'deposit_detected', 'deposit_credited', 'deposit_failed', 'lock', 'release', 'transfer', 'trade', 'withdrawal', 'conversion'));

ALTER TABLE balance_changes
    DROP CONSTRAINT balance_changes_change_type_check,
    ADD CONSTRAINT balance_changes_change_type_check CHECK (change_type IN (
        'PENDING_DEPOSIT', 'DEPOSIT', 'DEPOSIT_FAILED', 'LOCK', 'UNLOCK', 'TRADE_CREDIT', 'TRADE_DEBIT', 'FEE', 'WITHDRAWAL',
        'TRANSFER_OUT', 'TRANSFER_IN', 'CONVERSION_DEBIT', 'CONVERSION_CREDIT'));

CREATE OR REPLACE FUNCTION ledger_entry_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    entry uuid;
    unbalanced record;
    entry_kind text;
    shape text;
BEGIN
    IF TG_TABLE_NAME = 'ledger_entries' THEN
        entry := NEW.id;
    ELSE
        entry := NEW.entry_id;
    END IF;
    SELECT d.currency_hk, SUM(d.delta) AS total INTO unbalanced FROM (
        SELECT p.currency_hk, p.amount::numeric AS delta FROM ledger_postings p WHERE p.entry_id = entry
        UNION ALL
        SELECT c.currency_hk,
            (c.new_available::numeric + c.new_locked::numeric + c.new_pending::numeric)
            - (c.old_available::numeric + c.old_locked::numeric + c.old_pending::numeric)
        FROM balance_changes c WHERE c.entry_id = entry
    ) d GROUP BY d.currency_hk HAVING SUM(d.delta) <> 0 LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'ledger entry % does not balance in currency %: the postings sum to %', entry, unbalanced.currency_hk, unbalanced.total
            USING ERRCODE = 'check_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM balance_changes c WHERE c.entry_id = entry)
       AND NOT EXISTS (SELECT 1 FROM ledger_postings p WHERE p.entry_id = entry) THEN
        RAISE EXCEPTION 'ledger entry % moves nothing', entry USING ERRCODE = 'check_violation';
    END IF;
    -- The shape: the change types of the entry, then its postings as
    -- account and sign, each sorted, in the C collation so that the order
    -- depends on no setting of the database.
    SELECT e.kind INTO entry_kind FROM ledger_entries e WHERE e.id = entry;
    SELECT COALESCE((SELECT string_agg(c.change_type, ',' ORDER BY c.change_type COLLATE "C")
            FROM balance_changes c WHERE c.entry_id = entry), '')
        || '|' ||
        COALESCE((SELECT string_agg(s.posting, ',' ORDER BY s.posting COLLATE "C") FROM (
            SELECT p.account || CASE WHEN p.amount::numeric > 0 THEN '+' ELSE '-' END AS posting
            FROM ledger_postings p WHERE p.entry_id = entry) s), '')
        INTO shape;
    IF NOT shape = ANY (CASE entry_kind
        WHEN 'deposit_detected' THEN ARRAY['PENDING_DEPOSIT|deposits_pending-']
        WHEN 'deposit_credited' THEN ARRAY['DEPOSIT|custody-,deposits_pending+']
        WHEN 'deposit_failed' THEN ARRAY['DEPOSIT_FAILED|deposits_pending+']
        WHEN 'lock' THEN ARRAY['LOCK|']
        WHEN 'release' THEN ARRAY['UNLOCK|']
        WHEN 'transfer' THEN ARRAY['LOCK,TRANSFER_IN,TRANSFER_OUT|']
        WHEN 'trade' THEN ARRAY['TRADE_CREDIT,TRADE_DEBIT|inventory+,inventory-',
            'FEE,TRADE_CREDIT,TRADE_DEBIT|fees+,inventory+,inventory-']
        WHEN 'withdrawal' THEN ARRAY['WITHDRAWAL|custody+', 'FEE,WITHDRAWAL|custody+,fees+']
        WHEN 'conversion' THEN ARRAY['CONVERSION_CREDIT,CONVERSION_DEBIT,LOCK|custody+,custody-',
            'CONVERSION_CREDIT,CONVERSION_DEBIT,LOCK|custody+,custody-,fees+',
            'CONVERSION_DEBIT,LOCK|custody+,custody-,fees+']
        ELSE ARRAY[]::text[] END) THEN
        RAISE EXCEPTION 'ledger entry % of kind % has the shape %, which no operation of the ledger writes', entry, entry_kind, shape
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;

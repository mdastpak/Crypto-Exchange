-- The shape of a ledger entry, and the seal of the findings (#404).
--
-- The balance check of an entry accepted any entry whose postings sum to
-- zero, so that a role that may insert into the ledger could credit a user
-- and offset the credit against an account of the exchange, such as the
-- fees, with an entry that no operation of the ledger writes. Each kind of
-- entry now has the balance changes and the postings, by account and sign,
-- that its operation writes (internal/ledger), and nothing else commits.
-- The entries written before are left as they are; the reconciliation
-- checks the accounts of the exchange.
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
        ELSE ARRAY[]::text[] END) THEN
        RAISE EXCEPTION 'ledger entry % of kind % has the shape %, which no operation of the ledger writes', entry, entry_kind, shape
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;

-- A finding is the alarm of a row changed outside the backend. A role that
-- may write the findings could close one in SQL and hide the row from the
-- administrators, so the resolution of a finding is sealed too: a finding
-- closed without the backend's key is itself a finding. The findings of
-- before have no seal: the verifier seals them in report mode
-- (docs/data-protection.md#modes).
ALTER TABLE integrity_findings
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER;

ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups',
            'admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes',
            'balances', 'withdrawal_destinations', 'operation_confirmations', 'user_confirmation_methods',
            'user_anti_phishing_codes', 'sms_routing', 'currency_networks', 'markets', 'withdrawals',
            'integrity_findings'));

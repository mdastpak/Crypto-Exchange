-- The ledger (#60): every change of a balance is a posting of a ledger
-- entry, and the postings of an entry sum to zero in each currency, so that
-- money is never created or lost by the books.
--
-- An entry is one operation, such as a credited deposit or a settled trade,
-- named by an idempotency key: the same key never applies twice. Its
-- postings are of two kinds:
--
--   - the changes of the balances of users (balance_changes), each a
--     transition of the balance state machine (internal/domain/balance),
--     with the components before and after, and the version it made;
--   - the postings to the exchange's own accounts (ledger_postings): its
--     custody holdings, the fees it earned, its trading inventory, and the
--     deposits that are detected but not credited yet.
--
-- A user's position in a currency is available + locked + pending. In each
-- entry and currency, the changes of the positions plus the postings to the
-- exchange's accounts sum to zero, which a constraint trigger checks at
-- commit. The custody account holds the negative of what custody must hold:
-- the reconciliation compares its opposite with the holdings.
--
-- Entries, postings, balance changes, and snapshots are append-only.
--
-- Nothing wrote the balance tables, so they are empty; this migration
-- refuses to run otherwise.
DO $$
DECLARE
    t text;
    found boolean;
BEGIN
    FOREACH t IN ARRAY ARRAY['balances', 'balance_changes', 'balance_snapshots'] LOOP
        EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I)', t) INTO found;
        IF found THEN
            RAISE EXCEPTION 'table % holds rows, which no code wrote; the ledger (#60) starts from empty books', t;
        END IF;
    END LOOP;
END $$;

CREATE TABLE ledger_entries (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind text NOT NULL CONSTRAINT ledger_entries_kind_check CHECK (kind IN (
        'deposit_detected', 'deposit_credited', 'deposit_failed', 'lock', 'release', 'transfer', 'trade', 'withdrawal')),
    -- The key that the caller names the operation with, such as
    -- "deposit:tron:<transaction>:<output>". An operation retried with the
    -- same key and the same request returns the first result; with another
    -- request it is refused.
    idempotency_key text NOT NULL CONSTRAINT ledger_entries_idempotency_key_check CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    -- The SHA-256 of the request, which a retry must match.
    request_hash bytea NOT NULL CONSTRAINT ledger_entries_request_hash_check CHECK (octet_length(request_hash) = 32),
    -- What caused the entry, such as an order, a trade, a deposit, or a
    -- withdrawal, and its ID.
    reference_type text CONSTRAINT ledger_entries_reference_type_check CHECK (reference_type ~ '^[a-z_]{1,50}$'),
    reference_id text CONSTRAINT ledger_entries_reference_id_check CHECK (length(reference_id) BETWEEN 1 AND 200),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ledger_entries_idempotency_key_unique UNIQUE (idempotency_key),
    CONSTRAINT ledger_entries_reference_check CHECK ((reference_type IS NULL) = (reference_id IS NULL))
);

CREATE INDEX ledger_entries_reference ON ledger_entries (reference_type, reference_id) WHERE reference_id IS NOT NULL;
CREATE INDEX ledger_entries_created_at ON ledger_entries (created_at);

CREATE TABLE ledger_postings (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    entry_id uuid NOT NULL REFERENCES ledger_entries (id),
    -- custody: the negative of what custody must hold. fees: what the
    -- exchange earned. inventory: what the exchange holds to trade as the
    -- counterparty of users. deposits_pending: the negative of the deposits
    -- detected and not credited yet.
    account text NOT NULL CONSTRAINT ledger_postings_account_check CHECK (account IN ('custody', 'fees', 'inventory', 'deposits_pending')),
    currency_hk uuid NOT NULL REFERENCES currencies (hk),
    amount exact_decimal NOT NULL CONSTRAINT ledger_postings_amount_check CHECK (amount::numeric <> 0)
);

CREATE INDEX ledger_postings_entry ON ledger_postings (entry_id);
CREATE INDEX ledger_postings_account ON ledger_postings (currency_hk, account);

-- A balance change is a posting of its entry, and the version that it made,
-- so that the changes of a balance replay in order and none is applied
-- twice. The reference moves to the entry.
DROP INDEX idx_balance_changes_reference;
ALTER TABLE balance_changes
    DROP COLUMN reference_id,
    DROP COLUMN reference_type,
    ADD COLUMN entry_id uuid NOT NULL CONSTRAINT balance_changes_entry_id_fkey REFERENCES ledger_entries (id),
    ADD COLUMN version integer NOT NULL CONSTRAINT balance_changes_version_check CHECK (version >= 2),
    ADD CONSTRAINT balance_changes_balance_version_unique UNIQUE (balance_id, version),
    ADD CONSTRAINT balance_changes_change_type_check CHECK (change_type IN (
        'PENDING_DEPOSIT', 'DEPOSIT', 'DEPOSIT_FAILED', 'LOCK', 'UNLOCK', 'TRADE_CREDIT', 'TRADE_DEBIT', 'FEE', 'WITHDRAWAL',
        'TRANSFER_OUT', 'TRANSFER_IN'));

CREATE INDEX balance_changes_entry ON balance_changes (entry_id);

-- The balance check of an entry, at commit: every posting of the entry and
-- every change of a balance, of any table, triggers it, so that a row added
-- to an entry later is checked too.
CREATE FUNCTION ledger_entry_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    entry uuid;
    unbalanced record;
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
    RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER ledger_entries_balanced AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_entry_balanced();
CREATE CONSTRAINT TRIGGER ledger_postings_balanced AFTER INSERT ON ledger_postings
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_entry_balanced();
CREATE CONSTRAINT TRIGGER balance_changes_balanced AFTER INSERT ON balance_changes
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_entry_balanced();

-- The books are append-only, as audit_events is.
CREATE FUNCTION ledger_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not allowed', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'insufficient_privilege';
END $$;

CREATE TRIGGER ledger_entries_no_update_or_delete BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_append_only();
CREATE TRIGGER ledger_entries_no_truncate BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_append_only();
CREATE TRIGGER ledger_postings_no_update_or_delete BEFORE UPDATE OR DELETE ON ledger_postings
    FOR EACH ROW EXECUTE FUNCTION ledger_append_only();
CREATE TRIGGER ledger_postings_no_truncate BEFORE TRUNCATE ON ledger_postings
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_append_only();
CREATE TRIGGER balance_changes_no_update_or_delete BEFORE UPDATE OR DELETE ON balance_changes
    FOR EACH ROW EXECUTE FUNCTION ledger_append_only();
CREATE TRIGGER balance_changes_no_truncate BEFORE TRUNCATE ON balance_changes
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_append_only();
CREATE TRIGGER balance_snapshots_no_update_or_delete BEFORE UPDATE OR DELETE ON balance_snapshots
    FOR EACH ROW EXECUTE FUNCTION ledger_append_only();
CREATE TRIGGER balance_snapshots_no_truncate BEFORE TRUNCATE ON balance_snapshots
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_append_only();

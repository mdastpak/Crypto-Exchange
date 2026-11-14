-- The withdrawals of users (#70). The table of the baseline was shaped for
-- a crypto custody that never wrote it: an address, a tag, a fee level, and
-- a transaction hash in one row. A withdrawal is now a request of the user,
-- confirmed with the second factor (#161), to a destination of the user's
-- book (#160), on a network of its currency (#57), whose funds the ledger
-- locks when it is recorded (#60). Its payouts, by bank or on chain, are
-- rows of their own, later.
--
-- Nothing wrote the table, so it is empty; this migration refuses to run
-- otherwise.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM withdrawals) THEN
        RAISE EXCEPTION 'withdrawals holds rows; this migration replaces the table (#70)';
    END IF;
END $$;

DROP TABLE withdrawals;
DROP TYPE withdrawal_status_type;

CREATE TABLE withdrawals (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users (hk),
    currency_hk uuid NOT NULL,
    network character varying(50) NOT NULL,
    destination_id uuid NOT NULL REFERENCES withdrawal_destinations (id),
    -- What the destination receives, and the fee of the network; the
    -- ledger locks their sum.
    amount exact_decimal NOT NULL CONSTRAINT withdrawals_amount_check CHECK (amount::numeric > 0),
    fee exact_decimal NOT NULL CONSTRAINT withdrawals_fee_check CHECK (fee::numeric >= 0),
    status text NOT NULL CONSTRAINT withdrawals_status_check CHECK (status IN (
        'review', 'queued', 'processing', 'partially_paid', 'completed', 'cancelled', 'rejected', 'failed')),
    -- The key of the request, unique per user, and the digest of what it
    -- asked, so that a retry returns the same withdrawal and another
    -- request with the key is refused.
    idempotency_key character varying(100) NOT NULL,
    request_digest bytea NOT NULL,
    confirmation_id uuid NOT NULL REFERENCES operation_confirmations (id),
    -- Why it waits for an administrator, and who decided.
    review_reason text,
    reviewed_by uuid,
    reviewed_at timestamp with time zone,
    -- The reason of a rejection or a failure, for the user.
    reason text,
    created_at timestamp with time zone NOT NULL DEFAULT now(),
    updated_at timestamp with time zone NOT NULL DEFAULT now(),
    completed_at timestamp with time zone,
    cancelled_at timestamp with time zone,
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER,
    CONSTRAINT withdrawals_user_key UNIQUE (user_id, idempotency_key),
    CONSTRAINT withdrawals_confirmation_key UNIQUE (confirmation_id),
    CONSTRAINT withdrawals_network_fkey FOREIGN KEY (currency_hk, network) REFERENCES currency_networks (currency_hk, network)
);

CREATE INDEX withdrawals_user_created ON withdrawals (user_id, created_at DESC);
-- The withdrawals that are not settled, which the administrators and the
-- payouts look for.
CREATE INDEX withdrawals_open ON withdrawals (status, created_at)
    WHERE status IN ('review', 'queued', 'processing', 'partially_paid');
CREATE TRIGGER update_withdrawals_updated_at BEFORE UPDATE ON withdrawals
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups',
            'admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes',
            'balances', 'withdrawal_destinations', 'operation_confirmations', 'user_confirmation_methods',
            'user_anti_phishing_codes', 'sms_routing', 'currency_networks', 'markets', 'withdrawals'));

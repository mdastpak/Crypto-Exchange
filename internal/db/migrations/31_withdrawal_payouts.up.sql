-- The payouts of withdrawals (#70). A withdrawal is paid by one or more
-- transfers: a rial withdrawal may be split across the limits of the bank
-- rails (card, PAYA, SATNA, Pol, or account to account in one bank), and
-- across days by the daily limits of a destination; a crypto one is one
-- transfer on chain. Each transfer is a row, planned before anything is
-- sent, so that a restart resumes the plan rather than computing a new
-- one.
--
-- A payout carries its own idempotency key, which the provider receives,
-- so that a retry never pays twice: one whose result is unknown, such as
-- after a timeout, is asked of the provider before any new attempt.
CREATE TABLE withdrawal_payouts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    withdrawal_id uuid NOT NULL REFERENCES withdrawals (id),
    seq integer NOT NULL CONSTRAINT withdrawal_payouts_seq_check CHECK (seq > 0),
    rail text NOT NULL CONSTRAINT withdrawal_payouts_rail_check CHECK (rail IN ('card', 'paya', 'satna', 'pol', 'account', 'chain')),
    provider text NOT NULL,
    -- What the destination receives by this transfer, and the share of the
    -- withdrawal's fee that it charges.
    amount exact_decimal NOT NULL CONSTRAINT withdrawal_payouts_amount_check CHECK (amount::numeric > 0),
    fee exact_decimal NOT NULL CONSTRAINT withdrawal_payouts_fee_check CHECK (fee::numeric >= 0),
    status text NOT NULL CONSTRAINT withdrawal_payouts_status_check CHECK (status IN (
        'planned', 'submitted', 'completed', 'failed', 'released')),
    -- When it may be sent: later days hold what the daily limits do not
    -- allow today.
    scheduled_for TIMESTAMPTZ NOT NULL DEFAULT now(),
    idempotency_key text NOT NULL,
    provider_ref text,
    -- The bank's tracking number, or the hash of the transaction on chain.
    tracking_id text,
    attempts integer NOT NULL DEFAULT 0,
    -- The provider's last answer, without personal data.
    last_error text,
    submitted_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER,
    CONSTRAINT withdrawal_payouts_seq_key UNIQUE (withdrawal_id, seq),
    CONSTRAINT withdrawal_payouts_idempotency_key UNIQUE (idempotency_key)
);

-- The payouts that are due to be sent or checked.
CREATE INDEX withdrawal_payouts_open ON withdrawal_payouts (status, scheduled_for)
    WHERE status IN ('planned', 'submitted');
CREATE TRIGGER update_withdrawal_payouts_updated_at BEFORE UPDATE ON withdrawal_payouts
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups',
            'admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes',
            'balances', 'withdrawal_destinations', 'operation_confirmations', 'user_confirmation_methods',
            'user_anti_phishing_codes', 'sms_routing', 'currency_networks', 'markets', 'withdrawals', 'integrity_findings',
            'withdrawal_payouts'));

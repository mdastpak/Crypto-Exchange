-- The table of the baseline holds none of the withdrawals of the workflow:
-- refuse rather than lose them, as the up migration refuses.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM withdrawals) THEN
        RAISE EXCEPTION 'withdrawals holds rows of the workflow (#70); this migration would drop them';
    END IF;
END $$;

DELETE FROM integrity_findings WHERE table_name = 'withdrawals';
ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups',
            'admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes',
            'balances', 'withdrawal_destinations', 'operation_confirmations', 'user_confirmation_methods',
            'user_anti_phishing_codes', 'sms_routing', 'currency_networks', 'markets'));

DROP TABLE withdrawals;

CREATE TYPE withdrawal_status_type AS ENUM (
    'PENDING', 'BROADCAST', 'CONFIRMING', 'COMPLETED', 'FAILED', 'CANCELLED', 'REJECTED');

-- The table as migrations 17 and 20 left it.
CREATE TABLE withdrawals (
    id uuid DEFAULT uuid_generate_v4() NOT NULL,
    user_id uuid NOT NULL,
    sub_account_id uuid,
    currency_hk uuid NOT NULL,
    amount exact_decimal NOT NULL,
    address character varying(200) NOT NULL,
    tag character varying(100),
    fee_level fee_level DEFAULT 'NORMAL'::fee_level NOT NULL,
    custom_fee exact_decimal,
    subtract_fee boolean DEFAULT false NOT NULL,
    priority boolean DEFAULT false NOT NULL,
    idempotency_key character varying(100) NOT NULL,
    status withdrawal_status_type DEFAULT 'PENDING'::withdrawal_status_type NOT NULL,
    tx_hash character varying(200),
    estimated_fee exact_decimal DEFAULT 0 NOT NULL,
    actual_fee exact_decimal DEFAULT 0 NOT NULL,
    net_amount exact_decimal DEFAULT 0 NOT NULL,
    confirmations integer DEFAULT 0 NOT NULL,
    required_confirms integer DEFAULT 1 NOT NULL,
    broadcast_at timestamp with time zone,
    confirmed_at timestamp with time zone,
    error_message text,
    raw_data jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT withdrawals_pkey PRIMARY KEY (id),
    CONSTRAINT withdrawals_idempotency_key_key UNIQUE (idempotency_key),
    CONSTRAINT withdrawals_currency_hk_fkey FOREIGN KEY (currency_hk) REFERENCES currencies (hk),
    CONSTRAINT withdrawals_sub_account_owner_fkey FOREIGN KEY (sub_account_id, user_id) REFERENCES sub_accounts (id, user_id)
        ON DELETE CASCADE,
    CONSTRAINT withdrawals_user_id_fkey FOREIGN KEY (user_id) REFERENCES users (hk) ON DELETE CASCADE
);
CREATE INDEX idx_withdrawals_currency_hk ON withdrawals USING btree (currency_hk);
CREATE INDEX idx_withdrawals_created ON withdrawals USING btree (created_at);
CREATE INDEX idx_withdrawals_idempotency ON withdrawals USING btree (idempotency_key);
CREATE INDEX idx_withdrawals_status ON withdrawals USING btree (status);
CREATE INDEX idx_withdrawals_user_id ON withdrawals USING btree (user_id);
CREATE TRIGGER update_withdrawals_updated_at BEFORE UPDATE ON withdrawals
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- Confirmations of operations with the second factor (#161). Every
-- withdrawal is confirmed with a code before it runs: by SMS, by email, or
-- from an authenticator app, as the user chooses. A confirmation is bound
-- to its operation by the digest of what it does (the amount, the
-- currency, the destination), so that a code confirms that operation and
-- no other. It is confirmed once, consumed once by the operation, and
-- expires. The rows are sealed (#139): a confirmation marked confirmed in
-- SQL is refused, not run.
--
-- The method each user chose is a table of its own, sealed, rather than a
-- column of users: the seal of users then stays as it is, and a server can
-- still read the users of a schema that went back below this migration,
-- as `admin fields decrypt` does before migration 16 goes down.
CREATE TABLE user_confirmation_methods (
    user_hk uuid PRIMARY KEY REFERENCES users (hk),
    method text NOT NULL CONSTRAINT user_confirmation_methods_method_check CHECK (method IN ('sms', 'email', 'totp')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER
);

CREATE TABLE operation_confirmations (
    id uuid PRIMARY KEY,
    user_hk uuid NOT NULL REFERENCES users (hk),
    purpose text NOT NULL CONSTRAINT operation_confirmations_purpose_check CHECK (purpose IN ('withdrawal')),
    -- The SHA-256 of the operation, which its consumer computes again.
    digest bytea NOT NULL CONSTRAINT operation_confirmations_digest_check CHECK (octet_length(digest) = 32),
    -- What the user is told the code confirms: the amount, such as
    -- "100 USDT", and the destination, masked, such as "TR7NHq...gjLj6t",
    -- never a full number. Each fits a parameter of an SMS template, which
    -- some providers bound to 25 characters (#371).
    amount text NOT NULL CONSTRAINT operation_confirmations_amount_check CHECK (length(amount) BETWEEN 1 AND 25),
    destination text NOT NULL CONSTRAINT operation_confirmations_destination_check CHECK (length(destination) BETWEEN 1 AND 25),
    method text NOT NULL CONSTRAINT operation_confirmations_method_check CHECK (method IN ('sms', 'email', 'totp')),
    -- The session of the code of an SMS or email confirmation in the code
    -- store, and the contact it went to, masked.
    code_session text CONSTRAINT operation_confirmations_code_session_check CHECK (length(code_session) BETWEEN 1 AND 200),
    sent_to text CONSTRAINT operation_confirmations_sent_to_check CHECK (length(sent_to) BETWEEN 1 AND 200),
    failed_attempts integer NOT NULL DEFAULT 0 CONSTRAINT operation_confirmations_failed_attempts_check CHECK (failed_attempts >= 0),
    expires_at TIMESTAMPTZ NOT NULL,
    confirmed_at TIMESTAMPTZ,
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER,
    CONSTRAINT operation_confirmations_session_check CHECK ((method = 'totp') = (code_session IS NULL)),
    CONSTRAINT operation_confirmations_consumed_check CHECK (consumed_at IS NULL OR confirmed_at IS NOT NULL)
);

CREATE INDEX operation_confirmations_user ON operation_confirmations (user_hk, created_at);

ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups',
            'admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes',
            'balances', 'withdrawal_destinations', 'operation_confirmations', 'user_confirmation_methods'));

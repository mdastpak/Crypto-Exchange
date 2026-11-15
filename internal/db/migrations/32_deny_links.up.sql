-- The "this was not me" links (#374). Every message that carries the code
-- of a withdrawal confirmation (#161) carries a link too, and the link can
-- only stop and protect: it denies the confirmation, cancels the user's
-- withdrawals that are not being paid yet, ends every session of the user,
-- and starts the security hold (#251). It never approves anything, so a
-- mail scanner or a stranger who opens it can only lock the owner out of
-- withdrawals for a while.
--
-- The link carries a random token of 12 Base62 characters, short enough
-- for an SMS template (#371). The table holds only its SHA-256, bound to
-- the user and the confirmation. A token is used once and expires. The
-- rows are sealed from the first one: a link raised in SQL denies nothing.
CREATE TABLE deny_links (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash bytea NOT NULL CONSTRAINT deny_links_token_hash_check CHECK (octet_length(token_hash) = 32),
    user_hk uuid NOT NULL REFERENCES users (hk),
    confirmation_id uuid NOT NULL REFERENCES operation_confirmations (id),
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER,
    CONSTRAINT deny_links_token_hash_key UNIQUE (token_hash)
);

CREATE INDEX deny_links_confirmation ON deny_links (confirmation_id);

-- A denied confirmation can no longer be confirmed, sent again, or
-- consumed. The column is left out of the seal while it is NULL, so the
-- seals made before it still match.
ALTER TABLE operation_confirmations ADD COLUMN denied_at TIMESTAMPTZ;

ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups',
            'admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes',
            'balances', 'withdrawal_destinations', 'operation_confirmations', 'user_confirmation_methods',
            'user_anti_phishing_codes', 'sms_routing', 'currency_networks', 'markets', 'withdrawals', 'integrity_findings',
            'withdrawal_payouts', 'deny_links'));

-- The verification of a user's identity through a KYC provider (#158).
--
-- One person, one account (the owner's decision on #158): a verified
-- national ID gets a keyed blind index (#140), unique, so a second account
-- that submits a national ID verified for another is refused. The date of
-- birth, which the provider checks with the national ID and with the
-- owner of a card or an IBAN, is stored encrypted beside it. The row is
-- sealed (#139). Its rows from before this migration, if any, have neither
-- the index nor a date of birth, and are adopted by the verifier in report
-- mode like the rows of every table that was sealed later.
ALTER TABLE user_national_ids
    ADD COLUMN national_id_bidx BYTEA,
    ADD COLUMN birth_date_sealed BYTEA,
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER;
CREATE UNIQUE INDEX user_national_ids_bidx ON user_national_ids (national_id_bidx) WHERE national_id_bidx IS NOT NULL;

-- A check of a national ID and its date of birth, as a user submits them.
-- It is pending while the provider cannot answer, and the scheduler asks
-- again, never approving it on a failure; then it is matched, mismatched,
-- or taken, when the national ID is verified for another account. A
-- mismatch is final. The values are encrypted, and the row is sealed from
-- its first version.
CREATE TABLE identity_checks (
    id UUID PRIMARY KEY,
    user_hk UUID NOT NULL REFERENCES users (hk),
    national_id_sealed BYTEA NOT NULL,
    national_id_bidx BYTEA NOT NULL,
    birth_date_sealed BYTEA NOT NULL,
    status TEXT NOT NULL CONSTRAINT identity_checks_status_check CHECK (status IN ('pending', 'matched', 'mismatched', 'taken')),
    provider TEXT NOT NULL CONSTRAINT identity_checks_provider_check CHECK (length(provider) BETWEEN 1 AND 40),
    provider_ref TEXT CONSTRAINT identity_checks_provider_ref_check CHECK (length(provider_ref) BETWEEN 1 AND 200),
    attempts INTEGER NOT NULL DEFAULT 0 CONSTRAINT identity_checks_attempts_check CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    decided_at TIMESTAMPTZ,
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER,
    CONSTRAINT identity_checks_decided_check CHECK ((status = 'pending') = (decided_at IS NULL)),
    CONSTRAINT identity_checks_next_attempt_check CHECK ((status = 'pending') = (next_attempt_at IS NOT NULL))
);
-- One check of a user waits at a time.
CREATE UNIQUE INDEX identity_checks_user_pending ON identity_checks (user_hk) WHERE status = 'pending';
CREATE INDEX identity_checks_user ON identity_checks (user_hk, created_at DESC);
CREATE INDEX identity_checks_due ON identity_checks (next_attempt_at) WHERE status = 'pending';

-- A card or an IBAN whose ownership the provider could not check yet is
-- added pending, unusable, and checked again by the scheduler; a mismatch
-- then removes it. ownership_pending_since is sealed while it is set.
ALTER TABLE withdrawal_destinations
    ADD COLUMN ownership_pending_since TIMESTAMPTZ,
    ADD COLUMN ownership_attempts INTEGER NOT NULL DEFAULT 0 CONSTRAINT withdrawal_destinations_ownership_attempts_check CHECK (ownership_attempts >= 0),
    ADD COLUMN ownership_next_check TIMESTAMPTZ,
    DROP CONSTRAINT withdrawal_destinations_fiat_check,
    ADD CONSTRAINT withdrawal_destinations_fiat_check CHECK (kind = 'crypto'
        OR (memo_enc IS NULL AND ((verified_by IS NOT NULL) <> (ownership_pending_since IS NOT NULL)))),
    ADD CONSTRAINT withdrawal_destinations_ownership_check CHECK ((ownership_pending_since IS NULL) = (ownership_next_check IS NULL));
CREATE INDEX withdrawal_destinations_ownership_due ON withdrawal_destinations (ownership_next_check)
    WHERE ownership_pending_since IS NOT NULL AND removed_at IS NULL;

-- The new sealed tables may have findings; every table of before stays.
ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups',
            'admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes',
            'balances', 'withdrawal_destinations', 'operation_confirmations', 'user_confirmation_methods',
            'user_anti_phishing_codes', 'sms_routing', 'currency_networks', 'markets', 'withdrawals', 'integrity_findings',
            'withdrawal_payouts', 'deny_links', 'price_overrides',
            'currency_migrations', 'currency_delistings',
            'user_national_ids', 'identity_checks'));

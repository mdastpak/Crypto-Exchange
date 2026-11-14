-- The anti-phishing code of each user (#375): a phrase of their own that
-- every email from the exchange to their verified contacts shows, so that
-- an email without it, or with another, is not the exchange's. It is
-- stored encrypted only (#140) and sealed (#139). A removed one keeps its
-- row, emptied: the runtime role does not delete.
--
-- The exchange suggests a change of a code that is old or that many emails
-- have shown: shown_count counts the emails, and next_reminder_at is when
-- the reminder job next looks at the row. Neither is sealed, since every
-- email changes the count; changed in SQL, they only move a reminder.
CREATE TABLE user_anti_phishing_codes (
    user_hk uuid PRIMARY KEY REFERENCES users (hk),
    phrase_enc bytea,
    set_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    shown_count BIGINT NOT NULL DEFAULT 0,
    reminded_at TIMESTAMPTZ,
    next_reminder_at TIMESTAMPTZ NOT NULL DEFAULT 'infinity',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    removed_at TIMESTAMPTZ,
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER,
    CONSTRAINT user_anti_phishing_codes_removed_check CHECK ((removed_at IS NULL) = (phrase_enc IS NOT NULL)),
    CONSTRAINT user_anti_phishing_codes_shown_count_check CHECK (shown_count >= 0)
);

-- The reminder job reads the rows that are due, and only those.
CREATE INDEX user_anti_phishing_codes_reminder_idx ON user_anti_phishing_codes (next_reminder_at)
    WHERE removed_at IS NULL;

ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups',
            'admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes',
            'balances', 'withdrawal_destinations', 'operation_confirmations', 'user_confirmation_methods',
            'user_anti_phishing_codes'));

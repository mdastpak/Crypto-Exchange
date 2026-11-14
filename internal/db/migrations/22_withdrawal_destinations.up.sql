-- The withdrawal destinations of users (#160). A withdrawal goes only to a
-- destination of the user's book: a card, an IBAN (Sheba), or a bank account
-- of the user's own, whose ownership a provider verified against the user's
-- national ID (#158), or a crypto address that the user registered with
-- their second factor. Never to a free address.
--
-- The number of a card, an IBAN, an account, or an address is personal
-- data: it is stored encrypted only, with the field key (#140), and found
-- by a keyed blind index. The rows are sealed (#139), so that a destination
-- swapped in SQL is refused, not paid. A removed destination keeps its row,
-- for the audit trail and the withdrawals that named it.
CREATE TABLE withdrawal_destinations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_hk uuid NOT NULL REFERENCES users (hk),
    kind text NOT NULL CONSTRAINT withdrawal_destinations_kind_check CHECK (kind IN ('card', 'iban', 'account', 'crypto')),
    -- The number or the address, encrypted, and its blind index: of the
    -- number for fiat, of the network, the address, and the memo for
    -- crypto.
    value_enc bytea NOT NULL,
    value_bidx bytea NOT NULL,
    -- What the user is shown: the number with its middle hidden.
    masked text NOT NULL CONSTRAINT withdrawal_destinations_masked_check CHECK (length(masked) BETWEEN 1 AND 120),
    label text CONSTRAINT withdrawal_destinations_label_check CHECK (length(label) BETWEEN 1 AND 50),
    -- The bank of a fiat destination: from the card's prefix, the IBAN's
    -- bank code, or, for an account, the user's choice. NULL when the
    -- prefix is not known. The payouts prefer an account-to-account
    -- transfer in the same bank (#70).
    bank text CONSTRAINT withdrawal_destinations_bank_check CHECK (bank ~ '^[a-z_]{1,40}$'),
    -- The network of a crypto address, and its memo or tag, encrypted.
    network text CONSTRAINT withdrawal_destinations_network_check CHECK (network ~ '^[a-z0-9_]{1,40}$'),
    memo_enc bytea,
    -- Who verified a fiat destination's ownership, and their reference.
    verified_by text CONSTRAINT withdrawal_destinations_verified_by_check CHECK (length(verified_by) BETWEEN 1 AND 40),
    verification_ref text CONSTRAINT withdrawal_destinations_verification_ref_check CHECK (length(verification_ref) BETWEEN 1 AND 200),
    -- A new destination is used only from this time, so that a hijacked
    -- session cannot add one and withdraw to it at once.
    usable_from TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    removed_at TIMESTAMPTZ,
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER,
    CONSTRAINT withdrawal_destinations_crypto_check CHECK ((kind = 'crypto') = (network IS NOT NULL)),
    CONSTRAINT withdrawal_destinations_fiat_check CHECK (kind = 'crypto' OR (memo_enc IS NULL AND verified_by IS NOT NULL)),
    CONSTRAINT withdrawal_destinations_account_bank_check CHECK (kind <> 'account' OR bank IS NOT NULL)
);

CREATE INDEX withdrawal_destinations_user ON withdrawal_destinations (user_hk) WHERE removed_at IS NULL;

-- A card, an IBAN, or an account belongs to one person, so it is in one
-- book at a time. A crypto address may be in several books, such as the
-- deposit address of another exchange, but once in each.
CREATE UNIQUE INDEX withdrawal_destinations_fiat_unique ON withdrawal_destinations (kind, value_bidx)
    WHERE removed_at IS NULL AND kind <> 'crypto';
CREATE UNIQUE INDEX withdrawal_destinations_crypto_unique ON withdrawal_destinations (user_hk, value_bidx)
    WHERE removed_at IS NULL AND kind = 'crypto';

ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups',
            'admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes',
            'balances', 'withdrawal_destinations'));

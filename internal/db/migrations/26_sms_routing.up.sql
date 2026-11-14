-- The routing of SMS that administrators set (#371): auto, through the
-- configured providers with the unhealthy ones last, or manual, through an
-- order of their choice. One row at most; none is auto. It is sealed
-- (#139): a routing changed in SQL is ignored, and the auto mode applies.
CREATE TABLE sms_routing (
    id BIGINT PRIMARY KEY CHECK (id = 1),
    mode TEXT NOT NULL CHECK (mode IN ('auto', 'manual')),
    -- The providers in order, comma-separated, for the manual mode.
    provider_order TEXT NOT NULL DEFAULT '',
    updated_by uuid NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER,
    CONSTRAINT sms_routing_order_check CHECK ((mode = 'manual') = (provider_order <> ''))
);

ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups',
            'admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes',
            'balances', 'withdrawal_destinations', 'operation_confirmations', 'user_confirmation_methods',
            'user_anti_phishing_codes', 'sms_routing'));

-- The AML rules and their alerts (#73).
--
-- config/aml_rules.yaml is the source of the rules. At startup the server
-- syncs it into aml_rules, one row per rule by its code: a rule whose
-- definition changed gets the next rule_version, and each version is kept
-- in aml_rule_history, so an alert names the exact rule that raised it. A
-- rule that left the file is retired, never deleted. Every row is sealed
-- (#139): a rule disabled or loosened in SQL is refused, and nothing is
-- evaluated against it. No service wrote these tables before, so they are
-- sealed from their first row.
ALTER TABLE aml_rules
    ADD COLUMN code TEXT,
    ADD COLUMN rule_version INTEGER NOT NULL DEFAULT 1 CONSTRAINT aml_rules_rule_version_check CHECK (rule_version >= 1),
    ADD COLUMN applies_to TEXT[] NOT NULL DEFAULT '{}'
        CONSTRAINT aml_rules_applies_to_check CHECK (applies_to <@ ARRAY['deposit', 'withdrawal', 'trade']),
    ADD COLUMN retired_at TIMESTAMPTZ,
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER,
    ADD CONSTRAINT aml_rules_code_check CHECK (code ~ '^[a-z][a-z0-9_]{0,63}$'),
    ADD CONSTRAINT aml_rules_severity_check CHECK (severity IN ('LOW', 'MEDIUM', 'HIGH', 'CRITICAL'));
-- Rows from before the sync have no code; there are none, as nothing wrote
-- the table.
DELETE FROM aml_rules WHERE code IS NULL;
ALTER TABLE aml_rules ALTER COLUMN code SET NOT NULL;
CREATE UNIQUE INDEX aml_rules_code ON aml_rules (code);

-- Each version of each rule, as the sync wrote it, in its canonical JSON.
-- Never changed or deleted after its seal.
CREATE TABLE aml_rule_history (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_id UUID NOT NULL REFERENCES aml_rules (id),
    rule_version INTEGER NOT NULL CONSTRAINT aml_rule_history_rule_version_check CHECK (rule_version >= 1),
    definition JSONB NOT NULL CONSTRAINT aml_rule_history_definition_check CHECK (jsonb_typeof(definition) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER,
    CONSTRAINT aml_rule_history_key UNIQUE (rule_id, rule_version)
);

-- The operations that the rules evaluated: what the velocity, structuring,
-- and pattern rules count over a window. Each is written in the
-- transaction of the operation, so an operation that rolled back is not
-- counted. One that a rule refused is written apart, refused, for its
-- alerts, and is not counted either. value_usdt is its value at the time,
-- NULL when no price was current.
CREATE TABLE aml_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_hk UUID NOT NULL REFERENCES users (hk),
    kind TEXT NOT NULL CONSTRAINT aml_events_kind_check CHECK (kind IN ('deposit', 'withdrawal', 'trade')),
    ref TEXT NOT NULL CONSTRAINT aml_events_ref_check CHECK (length(ref) BETWEEN 1 AND 200),
    currency_hk UUID NOT NULL REFERENCES currencies (hk),
    amount exact_decimal NOT NULL CONSTRAINT aml_events_amount_check CHECK (amount::numeric > 0),
    value_usdt exact_decimal CONSTRAINT aml_events_value_usdt_check CHECK (value_usdt::numeric >= 0),
    counterparty TEXT CONSTRAINT aml_events_counterparty_check CHECK (length(counterparty) BETWEEN 1 AND 200),
    ip_country TEXT CONSTRAINT aml_events_ip_country_check CHECK (ip_country ~ '^[A-Z]{2}$'),
    counterparty_country TEXT CONSTRAINT aml_events_counterparty_country_check CHECK (counterparty_country ~ '^[A-Z]{2}$'),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    refused BOOLEAN NOT NULL DEFAULT FALSE,
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER
);
CREATE UNIQUE INDEX aml_events_key ON aml_events (kind, ref) WHERE NOT refused;
CREATE INDEX aml_events_user ON aml_events (user_hk, occurred_at DESC);
CREATE INDEX aml_events_recent ON aml_events (occurred_at);

-- An alert names the version of its rule, the operation that raised it,
-- if any, and the action that was taken. A batch alert has a dedupe key,
-- so that one window raises it once. Alerts are sealed from their first
-- row: one closed in SQL is detected.
ALTER TABLE aml_alerts
    ADD COLUMN rule_version INTEGER NOT NULL CONSTRAINT aml_alerts_rule_version_check CHECK (rule_version >= 1),
    ADD COLUMN event_id UUID REFERENCES aml_events (id),
    ADD COLUMN action public.aml_action NOT NULL,
    ADD COLUMN dedupe_key TEXT CONSTRAINT aml_alerts_dedupe_key_check CHECK (length(dedupe_key) BETWEEN 1 AND 200),
    ADD COLUMN escalated_at TIMESTAMPTZ,
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER,
    ADD CONSTRAINT aml_alerts_resolution_check CHECK (length(resolution) BETWEEN 1 AND 1000),
    ADD CONSTRAINT aml_alerts_resolved_check CHECK ((status IN ('RESOLVED', 'DISMISSED')) = (resolved_at IS NOT NULL)),
    ADD CONSTRAINT aml_alerts_resolution_set_check CHECK ((resolved_at IS NULL) = (resolution IS NULL));
CREATE UNIQUE INDEX aml_alerts_dedupe ON aml_alerts (dedupe_key) WHERE dedupe_key IS NOT NULL;
CREATE INDEX aml_alerts_open ON aml_alerts (created_at) WHERE status IN ('OPEN', 'INVESTIGATING', 'ESCALATED');
CREATE INDEX aml_alerts_user_rule ON aml_alerts (user_id, rule_id, created_at DESC);

-- A withdrawal waits for an administrator when an AML rule asks for a
-- review.
ALTER TABLE withdrawals
    ADD CONSTRAINT withdrawals_review_reason_check CHECK (review_reason IN ('amount', 'no_threshold', 'aml'));

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
            'otc_trades', 'otc_house_positions',
            'venue_reservations', 'venue_holds',
            'otc_routing_rules', 'otc_routing_usage',
            'otc_routed_orders',
            'user_national_ids', 'identity_checks',
            'aml_rules', 'aml_rule_history', 'aml_events', 'aml_alerts'));

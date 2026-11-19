DELETE FROM integrity_findings WHERE table_name IN ('aml_rules', 'aml_rule_history', 'aml_events', 'aml_alerts');
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
            'user_national_ids', 'identity_checks'));

-- A withdrawal held by an AML rule stays held, for the reason of an amount.
UPDATE withdrawals SET review_reason = 'amount' WHERE review_reason = 'aml';
ALTER TABLE withdrawals DROP CONSTRAINT withdrawals_review_reason_check;

-- The alerts, events, and rules that the engine wrote mean nothing to the
-- schema of before.
DELETE FROM aml_alerts;
DROP INDEX IF EXISTS aml_alerts_user_rule;
DROP INDEX IF EXISTS aml_alerts_open;
DROP INDEX IF EXISTS aml_alerts_dedupe;
ALTER TABLE aml_alerts
    DROP CONSTRAINT aml_alerts_resolution_set_check,
    DROP CONSTRAINT aml_alerts_resolved_check,
    DROP CONSTRAINT aml_alerts_resolution_check,
    DROP COLUMN seal_key_version,
    DROP COLUMN row_seal,
    DROP COLUMN row_version,
    DROP COLUMN escalated_at,
    DROP COLUMN dedupe_key,
    DROP COLUMN action,
    DROP COLUMN event_id,
    DROP COLUMN rule_version;

DROP TABLE aml_events;
DROP TABLE aml_rule_history;

DELETE FROM aml_rules;
DROP INDEX IF EXISTS aml_rules_code;
ALTER TABLE aml_rules
    DROP CONSTRAINT aml_rules_severity_check,
    DROP CONSTRAINT aml_rules_code_check,
    DROP COLUMN seal_key_version,
    DROP COLUMN row_seal,
    DROP COLUMN row_version,
    DROP COLUMN retired_at,
    DROP COLUMN applies_to,
    DROP COLUMN rule_version,
    DROP COLUMN code;

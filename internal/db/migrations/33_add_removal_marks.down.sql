-- Reverts migration 33. The marked rows go, as the release before it would
-- have deleted them.

CREATE OR REPLACE FUNCTION currency_symbols_follow() RETURNS trigger AS $$
DECLARE
    new_symbol TEXT := upper(NEW.info->>'symbol');
BEGIN
    IF TG_OP = 'UPDATE' AND upper(OLD.info->>'symbol') IS NOT DISTINCT FROM new_symbol THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        -- The symbol before stays reserved for this currency.
        UPDATE currency_symbols SET current = FALSE WHERE currency_hk = NEW.hk AND current;
    END IF;
    -- A currency can take back a symbol it had; any other held symbol is
    -- refused.
    INSERT INTO currency_symbols (symbol, currency_hk, current) VALUES (new_symbol, NEW.hk, TRUE)
    ON CONFLICT (symbol) DO UPDATE SET current = TRUE WHERE currency_symbols.currency_hk = EXCLUDED.currency_hk;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'the symbol % is reserved by another currency', new_symbol
            USING ERRCODE = 'unique_violation', CONSTRAINT = 'currency_symbols_reserved';
    END IF;
    RETURN NEW;
END
$$ LANGUAGE plpgsql;

DELETE FROM currency_symbols WHERE released_at IS NOT NULL;
ALTER TABLE currency_symbols DROP CONSTRAINT currency_symbols_pkey;
ALTER TABLE currency_symbols ADD PRIMARY KEY (symbol);
DROP INDEX currency_symbols_reserved;
ALTER TABLE currency_symbols
    DROP CONSTRAINT currency_symbols_released_not_current,
    DROP CONSTRAINT currency_symbols_released,
    DROP COLUMN released_by,
    DROP COLUMN released_at,
    DROP COLUMN id;

DELETE FROM currency_capability_overrides WHERE cleared_at IS NOT NULL;
ALTER TABLE currency_capability_overrides
    DROP CONSTRAINT currency_capability_overrides_cleared,
    DROP COLUMN cleared_by,
    DROP COLUMN cleared_at;

DELETE FROM user_contacts WHERE removed_at IS NOT NULL;
CREATE UNIQUE INDEX user_contacts_value_unique_all ON user_contacts (type, lower(value COLLATE "C"));
CREATE UNIQUE INDEX user_contacts_primary_unique_all ON user_contacts (user_hk, type) WHERE is_primary;
DROP INDEX user_contacts_value_unique;
DROP INDEX user_contacts_primary_unique;
ALTER INDEX user_contacts_value_unique_all RENAME TO user_contacts_value_unique;
ALTER INDEX user_contacts_primary_unique_all RENAME TO user_contacts_primary_unique;
ALTER TABLE user_contacts DROP COLUMN removed_at;

-- Removal marks (#248): three tables record a removal with a mark instead
-- of losing the row, so that the runtime role needs no DELETE on them. A
-- marked row is kept for the record and is invisible to every reader; the
-- unique indexes ignore it, so the value it held can be taken again.
--
-- This migration only makes room for the marks. The release that applies
-- it still deletes these rows, because the instances of the release before
-- it keep running while it rolls out and do not know the marks; the release
-- after it starts writing them.

-- A contact that its user removed.
ALTER TABLE user_contacts ADD COLUMN removed_at TIMESTAMPTZ;
-- The new indexes are built before the old ones go, so that no insert ever
-- runs without the unique checks.
CREATE UNIQUE INDEX user_contacts_value_unique_live ON user_contacts (type, lower(value COLLATE "C"))
    WHERE removed_at IS NULL;
CREATE UNIQUE INDEX user_contacts_primary_unique_live ON user_contacts (user_hk, type)
    WHERE is_primary AND removed_at IS NULL;
DROP INDEX user_contacts_value_unique;
DROP INDEX user_contacts_primary_unique;
ALTER INDEX user_contacts_value_unique_live RENAME TO user_contacts_value_unique;
ALTER INDEX user_contacts_primary_unique_live RENAME TO user_contacts_primary_unique;

-- An override that an administrator cleared. The row stays the one row of
-- its currency and capability; setting the override again clears the mark.
ALTER TABLE currency_capability_overrides
    ADD COLUMN cleared_at TIMESTAMPTZ,
    ADD COLUMN cleared_by UUID,
    ADD CONSTRAINT currency_capability_overrides_cleared CHECK ((cleared_at IS NULL) = (cleared_by IS NULL));

-- A previous symbol that an administrator released. The symbol no longer
-- names one row, so the rows get their own key, and only the symbols that
-- are not released are reserved.
ALTER TABLE currency_symbols
    ADD COLUMN id BIGINT GENERATED ALWAYS AS IDENTITY,
    ADD COLUMN released_at TIMESTAMPTZ,
    ADD COLUMN released_by UUID,
    ADD CONSTRAINT currency_symbols_released CHECK ((released_at IS NULL) = (released_by IS NULL)),
    ADD CONSTRAINT currency_symbols_released_not_current CHECK (released_at IS NULL OR NOT current);
CREATE UNIQUE INDEX currency_symbols_reserved ON currency_symbols (symbol) WHERE released_at IS NULL;
ALTER TABLE currency_symbols DROP CONSTRAINT currency_symbols_pkey;
ALTER TABLE currency_symbols ADD PRIMARY KEY (id);

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
    -- A currency can take back a symbol it holds; any other held symbol is
    -- refused. A released symbol is held by no one.
    INSERT INTO currency_symbols (symbol, currency_hk, current) VALUES (new_symbol, NEW.hk, TRUE)
    ON CONFLICT (symbol) WHERE released_at IS NULL
    DO UPDATE SET current = TRUE WHERE currency_symbols.currency_hk = EXCLUDED.currency_hk;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'the symbol % is reserved by another currency', new_symbol
            USING ERRCODE = 'unique_violation', CONSTRAINT = 'currency_symbols_reserved';
    END IF;
    RETURN NEW;
END
$$ LANGUAGE plpgsql;

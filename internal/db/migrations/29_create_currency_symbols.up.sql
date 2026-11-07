-- The symbols of currencies (#215): the current symbol of each currency and
-- the symbols it had before a rebrand. A symbol names one currency at a
-- time: the primary key reserves a previous symbol, as it does a current
-- one, so that no other currency takes it until an administrator releases
-- it. A trigger keeps the table in step with currencies, in the transaction
-- of every change, so no writer, on any instance, can skip it.
CREATE TABLE currency_symbols (
    symbol      TEXT        PRIMARY KEY,
    currency_hk UUID        NOT NULL REFERENCES currencies (hk),
    current     BOOLEAN     NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_currency_symbols_current ON currency_symbols (currency_hk) WHERE current;
CREATE INDEX idx_currency_symbols_currency ON currency_symbols (currency_hk, created_at DESC);

INSERT INTO currency_symbols (symbol, currency_hk, current, created_at)
SELECT upper(info->>'symbol'), hk, TRUE, created_at FROM currencies
ON CONFLICT (symbol) DO NOTHING;

CREATE FUNCTION currency_symbols_follow() RETURNS trigger AS $$
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

CREATE TRIGGER currency_symbols_follow AFTER INSERT OR UPDATE OF info ON currencies
    FOR EACH ROW EXECUTE FUNCTION currency_symbols_follow();

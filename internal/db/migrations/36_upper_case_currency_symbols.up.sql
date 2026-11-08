-- Currency symbols in upper case (#126). Since #107 the backend stores every
-- symbol in upper case, but rows written before may hold `usdt` or `Usdt`.
-- Such a row fails validation on any update until its symbol is corrected,
-- and the unique index on the symbol is case-sensitive, so `usdt` and `USDT`
-- could both exist.
--
-- This migration upper-cases the symbols, and a CHECK keeps them so. With
-- every symbol in upper case, the existing unique index on info->>'symbol'
-- is case-insensitive in effect, and the queries that use it keep using it.
--
-- Rolling out: every server version since #107 already writes upper case,
-- so instances of the previous release keep working while this one and the
-- migration roll out. The public currency list in Redis may show a changed
-- symbol in its old case until its TTL (redis.currency_cache_ttl_seconds)
-- runs out.
--
-- Preflight: symbols that differ only in case, deleted currencies included
-- (the unique index covers them), and symbols that a rebrand reserved for
-- another currency cannot be merged by a migration. Stop with an actionable
-- message before any change. The failed attempt leaves version 36 marked
-- dirty, so after fixing the data run `migrate force 35` and start the server
-- again.
DO $$
DECLARE
    clashes  TEXT;
    reserved TEXT;
BEGIN
    SELECT string_agg(symbols, '; ' ORDER BY symbols) INTO clashes
    FROM (
        SELECT string_agg(info->>'symbol', ', ' ORDER BY info->>'symbol') AS symbols
        FROM currencies
        WHERE info->>'symbol' IS NOT NULL
        GROUP BY upper(info->>'symbol')
        HAVING COUNT(*) > 1
    ) AS groups;
    IF clashes IS NOT NULL THEN
        RAISE EXCEPTION 'migration 36: currency symbols differ only in case: %; change the symbol of all but one currency of each group (deleted currencies included), then run "migrate force 35" and start the server again', clashes;
    END IF;

    SELECT string_agg(c.info->>'symbol', ', ' ORDER BY c.info->>'symbol') INTO reserved
    FROM currencies c
    JOIN currency_symbols s ON s.symbol = upper(c.info->>'symbol')
        AND s.released_at IS NULL AND s.currency_hk <> c.hk;
    IF reserved IS NOT NULL THEN
        RAISE EXCEPTION 'migration 36: currency symbols are reserved by another currency once in upper case: %; change these symbols, or release the reservations with DELETE /admin/currencies/{hk}/symbols/{symbol}, then run "migrate force 35" and start the server again', reserved;
    END IF;
END
$$;

-- The change is a write like any other: the version is raised, so that an
-- administrator's update based on the old symbol is refused with 409 instead
-- of silently overwriting it. The currency_symbols_follow trigger sees the
-- same upper-case symbol before and after, and leaves the reservations
-- alone.
UPDATE currencies
SET info = jsonb_set(info, '{symbol}', to_jsonb(upper(info->>'symbol'))),
    version = version + 1,
    updated_at = NOW()
WHERE info->>'symbol' <> upper(info->>'symbol');

-- Migration 29 skipped the reservation of a symbol that another row held in
-- another case. Now that no two symbols clash, give every currency without
-- a current reservation its own, as the trigger would have.
INSERT INTO currency_symbols (symbol, currency_hk, current, created_at)
SELECT upper(c.info->>'symbol'), c.hk, TRUE, c.created_at
FROM currencies c
WHERE c.info->>'symbol' IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM currency_symbols s WHERE s.currency_hk = c.hk AND s.current)
ON CONFLICT (symbol) WHERE released_at IS NULL
DO UPDATE SET current = TRUE WHERE currency_symbols.currency_hk = EXCLUDED.currency_hk;

ALTER TABLE currencies ADD CONSTRAINT currencies_symbol_upper_case
    CHECK (info->>'symbol' = upper(info->>'symbol'));

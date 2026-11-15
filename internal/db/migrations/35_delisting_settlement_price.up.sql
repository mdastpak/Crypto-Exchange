-- The settlement of delistings (#216): the price that converts the
-- balances left at frozen, recorded once when the settlement starts, with
-- where it came from and when. The conversions are ledger entries of kind
-- conversion (migration 34) that refer to the delisting.
ALTER TABLE currency_delistings
    ADD COLUMN settlement_price exact_decimal,
    ADD COLUMN settlement_price_detail text,
    ADD COLUMN settlement_priced_at timestamp with time zone,
    ADD CONSTRAINT currency_delistings_settlement_price_check CHECK (
        (settlement_price IS NULL OR (settlement = 'convert' AND settlement_price::numeric > 0))
        AND (settlement_price IS NULL) = (settlement_priced_at IS NULL)
        AND (settlement_price IS NULL) = (settlement_price_detail IS NULL)
        AND length(settlement_price_detail) <= 200);

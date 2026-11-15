ALTER TABLE currency_delistings
    DROP CONSTRAINT currency_delistings_settlement_price_check,
    DROP COLUMN settlement_priced_at,
    DROP COLUMN settlement_price_detail,
    DROP COLUMN settlement_price;

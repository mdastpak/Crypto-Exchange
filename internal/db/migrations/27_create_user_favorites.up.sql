-- The personal favorites of users (#217): currencies and trading pairs, in
-- the order each user chose. A favorite keeps its row when its currency or
-- pair is later suspended or deleted, and is shown with that state.
CREATE TABLE user_favorites (
    user_hk     UUID        NOT NULL REFERENCES users (hk) ON DELETE CASCADE,
    kind        TEXT        NOT NULL CHECK (kind IN ('currency', 'pair')),
    target_hk   UUID        NOT NULL,
    currency_hk UUID        REFERENCES currencies (hk),
    market_id   UUID        REFERENCES markets (id),
    position    INTEGER     NOT NULL CHECK (position >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_hk, kind, target_hk),
    CHECK (
        (kind = 'currency' AND currency_hk = target_hk AND market_id IS NULL)
        OR (kind = 'pair' AND market_id = target_hk AND currency_hk IS NULL)
    )
);

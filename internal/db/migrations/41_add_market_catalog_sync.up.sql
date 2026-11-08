-- The catalog sync (#266). details_fetched_at says when the full record of
-- an asset (description, links, contracts) was last read from a source, so
-- that each run reads a bounded number of records, the missing and the
-- oldest first.
ALTER TABLE market_asset_sources ADD COLUMN details_fetched_at TIMESTAMPTZ;
CREATE INDEX market_asset_sources_details ON market_asset_sources (source, details_fetched_at NULLS FIRST);

-- What the sync could not decide alone, for an administrator (#270): a
-- contract or an identity that a source gives to another asset than the
-- catalog does, or an asset of a secondary source whose symbol an asset of
-- the catalog already has without proof that they are the same. Rows are
-- resolved, never deleted.
CREATE TABLE market_identity_conflicts (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source        TEXT NOT NULL,
    external_id   TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('contract', 'identity', 'symbol')),
    -- subject is what conflicts: network:address for a contract, the
    -- symbol for a symbol.
    subject       TEXT NOT NULL DEFAULT '',
    -- asset_id is the asset the catalog has; claimed_by, when known, the
    -- asset that the source's record belongs to.
    asset_id      BIGINT REFERENCES market_assets (id),
    claimed_by    BIGINT REFERENCES market_assets (id),
    detail        TEXT NOT NULL DEFAULT '' CHECK (length(detail) <= 1000),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at   TIMESTAMPTZ,
    resolved_note TEXT NOT NULL DEFAULT ''
);

-- One open conflict per source record and subject.
CREATE UNIQUE INDEX market_identity_conflicts_open
    ON market_identity_conflicts (source, external_id, kind, subject) WHERE resolved_at IS NULL;

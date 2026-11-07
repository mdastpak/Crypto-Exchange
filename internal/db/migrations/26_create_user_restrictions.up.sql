-- Restrictions of users for compliance (#220): a freeze of the account, a
-- block of operations, or a hold of one currency. A restriction applies from
-- its creation until it is lifted or reaches ends_at; rows are never
-- deleted, so that the history stays.
CREATE TABLE user_restrictions (
    hk          UUID        PRIMARY KEY,
    user_hk     UUID        NOT NULL REFERENCES users (hk),
    scope       TEXT        NOT NULL CHECK (scope IN ('account', 'operation', 'asset')),
    operations  TEXT[]      NOT NULL DEFAULT '{}'
        CHECK (operations <@ ARRAY['deposit', 'withdraw', 'convert', 'buy', 'sell', 'trade']::TEXT[]),
    currency_hk UUID        REFERENCES currencies (hk),
    reason      TEXT        NOT NULL CHECK (reason ~ '^[a-z_]+$'),
    case_ref    TEXT        CHECK (length(case_ref) <= 64),
    note        TEXT        CHECK (length(note) <= 1000),
    ends_at     TIMESTAMPTZ,
    created_by  UUID        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lifted_at   TIMESTAMPTZ,
    lifted_by   UUID,
    lift_note   TEXT        CHECK (length(lift_note) <= 1000),
    version     BIGINT      NOT NULL DEFAULT 1,
    CHECK ((scope = 'asset') = (currency_hk IS NOT NULL)),
    CHECK ((scope = 'account') = (cardinality(operations) = 0)),
    CHECK (ends_at IS NULL OR ends_at > created_at),
    CHECK ((lifted_at IS NULL) = (lifted_by IS NULL))
);

-- Every check reads the restrictions of one user that are not lifted.
CREATE INDEX idx_user_restrictions_open ON user_restrictions (user_hk) WHERE lifted_at IS NULL;
CREATE INDEX idx_user_restrictions_user_created ON user_restrictions (user_hk, created_at DESC, hk DESC);

-- The approval of the price overrides (#453). An override whose mid is
-- beyond price.overrides.max_deviation_bps of the live reference mid, or
-- for a market without one, waits for a second administrator until
-- approve_by, and lapses otherwise: it is served only once approved. The
-- columns are NULL for an override that applied at once, and for those
-- made before, so that their seals still match (#139).
ALTER TABLE price_overrides
    -- The live reference mid when it was set, NULL without one.
    ADD COLUMN reference_mid exact_decimal,
    -- Until when it waits for its approval; NULL for one that applied at
    -- once.
    ADD COLUMN approve_by TIMESTAMPTZ,
    ADD COLUMN approved_by uuid,
    ADD COLUMN approved_at TIMESTAMPTZ,
    ADD CONSTRAINT price_overrides_approval_check CHECK (
        (approved_by IS NULL) = (approved_at IS NULL)
        AND (approved_by IS NULL OR approve_by IS NOT NULL)
        AND (approve_by IS NULL OR approve_by > created_at)
        AND (approved_at IS NULL OR approved_at <= approve_by)),
    -- Never the administrator who set it.
    ADD CONSTRAINT price_overrides_approver_check CHECK (approved_by IS DISTINCT FROM created_by),
    ADD CONSTRAINT price_overrides_reference_check CHECK (reference_mid IS NULL OR reference_mid::numeric > 0);

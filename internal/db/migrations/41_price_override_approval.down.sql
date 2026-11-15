ALTER TABLE price_overrides
    DROP CONSTRAINT IF EXISTS price_overrides_reference_check,
    DROP CONSTRAINT IF EXISTS price_overrides_approver_check,
    DROP CONSTRAINT IF EXISTS price_overrides_approval_check,
    DROP COLUMN IF EXISTS approved_at,
    DROP COLUMN IF EXISTS approved_by,
    DROP COLUMN IF EXISTS approve_by,
    DROP COLUMN IF EXISTS reference_mid;

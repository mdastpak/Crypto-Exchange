-- Back to migration 47: the cursor of the fees goes, and the next read
-- starts again from its first window.
DELETE FROM venue_snapshots WHERE kind = 'fees';
ALTER TABLE venue_snapshots
    DROP CONSTRAINT venue_snapshots_kind_check,
    ADD CONSTRAINT venue_snapshots_kind_check CHECK (kind IN ('balances', 'instruments', 'networks', 'reconciliation'));

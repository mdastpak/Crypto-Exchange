-- The fees that a venue reports are read from a cursor (#503): the time
-- of the last read is kept as a snapshot of kind fees, and its failures as
-- those of the other snapshots.
ALTER TABLE venue_snapshots
    DROP CONSTRAINT venue_snapshots_kind_check,
    ADD CONSTRAINT venue_snapshots_kind_check CHECK (kind IN ('balances', 'instruments', 'networks', 'reconciliation', 'fees'));

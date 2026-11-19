-- A released reservation still counts against its balance until a balance
-- snapshot newer than its release (#500), so the reservations of a balance
-- are read by their release too. The index of the open ones stays for the
-- reservations that are not released.
CREATE INDEX venue_reservations_released ON venue_reservations (venue, asset, released_at)
    WHERE released_at IS NOT NULL;

-- The review of the routed orders (#513): an order left to a review
-- ends resolved, by an administrator who hedged it by hand or routed its
-- remainder again; each resolution is an event of the order and an audit
-- event.
ALTER TABLE otc_routed_orders
    DROP CONSTRAINT otc_routed_orders_status_check,
    ADD CONSTRAINT otc_routed_orders_status_check CHECK (status IN (
        'planned', 'placed', 'partially_filled', 'filled', 'withdrawal_requested', 'withdrawn', 'confirmed',
        'failed', 'cancelled', 'manual_review', 'resolved'));

-- The orders that wait for a person, which the review lists and the
-- metrics count at every scrape.
CREATE INDEX otc_routed_orders_review ON otc_routed_orders (venue, created_at)
    WHERE status = 'manual_review';

-- The fills of the last hour, of which the metrics give the time from the
-- plan to the fill.
CREATE INDEX otc_routed_order_events_filled ON otc_routed_order_events (at)
    WHERE to_status = 'filled';

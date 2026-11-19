DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM otc_routed_orders WHERE status = 'resolved') THEN
        RAISE EXCEPTION 'otc_routed_orders holds resolved orders; migration 57 cannot be reverted (#513)';
    END IF;
END $$;

DROP INDEX otc_routed_order_events_filled;
DROP INDEX otc_routed_orders_review;

ALTER TABLE otc_routed_orders
    DROP CONSTRAINT otc_routed_orders_status_check,
    ADD CONSTRAINT otc_routed_orders_status_check CHECK (status IN (
        'planned', 'placed', 'partially_filled', 'filled', 'withdrawal_requested', 'withdrawn', 'confirmed',
        'failed', 'cancelled', 'manual_review'));

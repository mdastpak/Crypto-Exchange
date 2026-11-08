-- The previous constraint cannot hold a control of market_data, and the
-- history of controls is never rewritten, so this refuses to run while one
-- exists.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM platform_controls WHERE feature = 'market_data') THEN
        RAISE EXCEPTION 'platform_controls holds lockdowns of market_data; the previous schema cannot keep them';
    END IF;
END
$$;
ALTER TABLE platform_controls DROP CONSTRAINT platform_controls_feature_check;
ALTER TABLE platform_controls ADD CONSTRAINT platform_controls_feature_check
    CHECK (feature IN ('sign_up', 'sign_in', 'profile_changes', 'deposits', 'withdrawals', 'trading',
                       'api_keys', 'websocket', 'read_only', 'scheduler', 'outbox'));
DROP TABLE IF EXISTS market_job_leases;

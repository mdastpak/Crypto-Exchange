-- The daily withdrawal limits of the verification levels (#157) sum, in the
-- transaction of each withdrawal request, the user's locks and releases of
-- one currency in the last 24 hours (ledger.Withdrawn). This index finds
-- them by user, currency, and time, so the check reads the user's recent
-- locks only, whatever the volume of the exchange. Orders lock funds too,
-- so their locks share it; trades, deposits, and the other changes do not.
CREATE INDEX balance_changes_user_locks ON balance_changes (user_id, currency_hk, "timestamp")
    WHERE change_type IN ('LOCK', 'UNLOCK');

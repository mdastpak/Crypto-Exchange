ALTER TABLE settlements
    DROP CONSTRAINT IF EXISTS settlements_to_sub_account_owner_fkey,
    DROP CONSTRAINT IF EXISTS settlements_from_sub_account_owner_fkey,
    ADD CONSTRAINT settlements_from_sub_account_id_fkey FOREIGN KEY (from_sub_account_id) REFERENCES public.sub_accounts(id),
    ADD CONSTRAINT settlements_to_sub_account_id_fkey FOREIGN KEY (to_sub_account_id) REFERENCES public.sub_accounts(id);

ALTER TABLE sessions
    DROP CONSTRAINT IF EXISTS sessions_sub_account_owner_fkey,
    ADD CONSTRAINT sessions_sub_account_id_fkey FOREIGN KEY (sub_account_id) REFERENCES public.sub_accounts(id) ON DELETE SET NULL;

ALTER TABLE withdrawals
    DROP CONSTRAINT IF EXISTS withdrawals_sub_account_owner_fkey,
    ADD CONSTRAINT withdrawals_sub_account_id_fkey FOREIGN KEY (sub_account_id) REFERENCES public.sub_accounts(id) ON DELETE CASCADE;

ALTER TABLE deposit_addresses
    DROP CONSTRAINT IF EXISTS deposit_addresses_sub_account_owner_fkey,
    ADD CONSTRAINT deposit_addresses_sub_account_id_fkey FOREIGN KEY (sub_account_id) REFERENCES public.sub_accounts(id) ON DELETE CASCADE;

ALTER TABLE aml_alerts
    DROP CONSTRAINT IF EXISTS aml_alerts_sub_account_owner_fkey,
    ADD CONSTRAINT aml_alerts_sub_account_id_fkey FOREIGN KEY (sub_account_id) REFERENCES public.sub_accounts(id);

ALTER TABLE orders
    DROP CONSTRAINT IF EXISTS orders_sub_account_owner_fkey,
    ADD CONSTRAINT fk_orders_sub_account FOREIGN KEY (sub_account_id) REFERENCES public.sub_accounts(id);

ALTER TABLE balance_snapshots
    DROP CONSTRAINT IF EXISTS balance_snapshots_sub_account_owner_fkey,
    ADD CONSTRAINT fk_balance_snapshots_sub_account FOREIGN KEY (sub_account_id) REFERENCES public.sub_accounts(id);

ALTER TABLE balance_changes
    DROP CONSTRAINT IF EXISTS balance_changes_sub_account_owner_fkey,
    ADD CONSTRAINT fk_balance_changes_sub_account FOREIGN KEY (sub_account_id) REFERENCES public.sub_accounts(id);

ALTER TABLE balances
    DROP CONSTRAINT IF EXISTS balances_sub_account_owner_fkey,
    ADD CONSTRAINT fk_balances_sub_account FOREIGN KEY (sub_account_id) REFERENCES public.sub_accounts(id);

ALTER TABLE sub_accounts DROP CONSTRAINT IF EXISTS sub_accounts_id_user_unique;

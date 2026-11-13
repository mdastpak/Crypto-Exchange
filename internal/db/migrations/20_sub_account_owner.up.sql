-- A sub-account belongs to one user (#63, #60). Every row that names a
-- user and a sub-account must name a sub-account of that user: a balance,
-- an order, or a withdrawal of user A in a sub-account of user B would move
-- money between two people. The foreign keys to sub_accounts checked only
-- that the sub-account exists; they now check its owner too. A row without a
-- sub-account is of the user's main account, and is not checked (MATCH
-- SIMPLE). Each key keeps the action on delete that it had.
ALTER TABLE sub_accounts ADD CONSTRAINT sub_accounts_id_user_unique UNIQUE (id, user_id);

ALTER TABLE balances
    DROP CONSTRAINT fk_balances_sub_account,
    ADD CONSTRAINT balances_sub_account_owner_fkey FOREIGN KEY (sub_account_id, user_id) REFERENCES sub_accounts (id, user_id);

ALTER TABLE balance_changes
    DROP CONSTRAINT fk_balance_changes_sub_account,
    ADD CONSTRAINT balance_changes_sub_account_owner_fkey FOREIGN KEY (sub_account_id, user_id) REFERENCES sub_accounts (id, user_id);

ALTER TABLE balance_snapshots
    DROP CONSTRAINT fk_balance_snapshots_sub_account,
    ADD CONSTRAINT balance_snapshots_sub_account_owner_fkey FOREIGN KEY (sub_account_id, user_id) REFERENCES sub_accounts (id, user_id);

ALTER TABLE orders
    DROP CONSTRAINT fk_orders_sub_account,
    ADD CONSTRAINT orders_sub_account_owner_fkey FOREIGN KEY (sub_account_id, user_id) REFERENCES sub_accounts (id, user_id);

ALTER TABLE aml_alerts
    DROP CONSTRAINT aml_alerts_sub_account_id_fkey,
    ADD CONSTRAINT aml_alerts_sub_account_owner_fkey FOREIGN KEY (sub_account_id, user_id) REFERENCES sub_accounts (id, user_id);

ALTER TABLE deposit_addresses
    DROP CONSTRAINT deposit_addresses_sub_account_id_fkey,
    ADD CONSTRAINT deposit_addresses_sub_account_owner_fkey FOREIGN KEY (sub_account_id, user_id) REFERENCES sub_accounts (id, user_id)
        ON DELETE CASCADE;

ALTER TABLE withdrawals
    DROP CONSTRAINT withdrawals_sub_account_id_fkey,
    ADD CONSTRAINT withdrawals_sub_account_owner_fkey FOREIGN KEY (sub_account_id, user_id) REFERENCES sub_accounts (id, user_id)
        ON DELETE CASCADE;

-- A session of a deleted sub-account goes on as a session of the user's
-- main account: only the sub-account is cleared, never the user.
ALTER TABLE sessions
    DROP CONSTRAINT sessions_sub_account_id_fkey,
    ADD CONSTRAINT sessions_sub_account_owner_fkey FOREIGN KEY (sub_account_id, user_id) REFERENCES sub_accounts (id, user_id)
        ON DELETE SET NULL (sub_account_id);

ALTER TABLE settlements
    DROP CONSTRAINT settlements_from_sub_account_id_fkey,
    DROP CONSTRAINT settlements_to_sub_account_id_fkey,
    ADD CONSTRAINT settlements_from_sub_account_owner_fkey FOREIGN KEY (from_sub_account_id, from_user_id) REFERENCES sub_accounts (id, user_id),
    ADD CONSTRAINT settlements_to_sub_account_owner_fkey FOREIGN KEY (to_sub_account_id, to_user_id) REFERENCES sub_accounts (id, user_id);

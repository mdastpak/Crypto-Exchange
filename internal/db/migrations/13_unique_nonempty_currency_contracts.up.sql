-- Native coins and fiat currencies have no contract and store an empty
-- string. A unique index over every value allowed only one of them, so only
-- non-empty contracts are unique.
DROP INDEX IF EXISTS currencies_info_contract_unique;
CREATE UNIQUE INDEX currencies_info_contract_unique ON currencies ((info->>'contract'))
    WHERE info->>'contract' <> '';

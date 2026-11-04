-- Restoring the index over every value fails while several currencies have
-- an empty contract. Give them distinct contracts before rolling back.
DROP INDEX IF EXISTS currencies_info_contract_unique;
CREATE UNIQUE INDEX currencies_info_contract_unique ON currencies ((info->>'contract'));

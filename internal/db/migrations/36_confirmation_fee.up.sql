-- The network fee that a confirmation told the user about (#408), such as
-- "1 USDT", so that a code sent again and the record of the confirmation
-- show what was confirmed. It is NULL for an operation without a fee, and
-- for the confirmations made before this migration. The column is sealed
-- (#139) once it holds a value and left out of the seal while it is NULL,
-- so the seals made before it still match.
ALTER TABLE operation_confirmations
    ADD COLUMN fee text CONSTRAINT operation_confirmations_fee_check CHECK (length(fee) BETWEEN 1 AND 25);

-- The symbols stay in upper case: their earlier case is not kept, and every
-- server version since #107 reads and writes upper case anyway. The
-- reservations added for currencies that had none stay too, as the trigger
-- would have made them.
ALTER TABLE currencies DROP CONSTRAINT currencies_symbol_upper_case;

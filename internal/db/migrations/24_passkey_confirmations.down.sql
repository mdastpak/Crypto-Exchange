-- The confirmations and choices by passkey go with the method.
DELETE FROM operation_confirmations WHERE method = 'passkey';
DELETE FROM user_confirmation_methods WHERE method = 'passkey';

ALTER TABLE operation_confirmations
    DROP CONSTRAINT operation_confirmations_method_check,
    ADD CONSTRAINT operation_confirmations_method_check CHECK (method IN ('sms', 'email', 'totp')),
    DROP CONSTRAINT operation_confirmations_session_check,
    ADD CONSTRAINT operation_confirmations_session_check CHECK ((method = 'totp') = (code_session IS NULL));

ALTER TABLE user_confirmation_methods
    DROP CONSTRAINT user_confirmation_methods_method_check,
    ADD CONSTRAINT user_confirmation_methods_method_check CHECK (method IN ('sms', 'email', 'totp'));

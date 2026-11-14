-- Passkeys confirm withdrawals too (#373), the one method that resists
-- phishing: the device signs only for the exchange's own domain. A
-- confirmation by passkey has no code in the code store, like one by the
-- authenticator app.
ALTER TABLE operation_confirmations
    DROP CONSTRAINT operation_confirmations_method_check,
    ADD CONSTRAINT operation_confirmations_method_check CHECK (method IN ('sms', 'email', 'totp', 'passkey')),
    DROP CONSTRAINT operation_confirmations_session_check,
    ADD CONSTRAINT operation_confirmations_session_check CHECK ((method IN ('totp', 'passkey')) = (code_session IS NULL));

ALTER TABLE user_confirmation_methods
    DROP CONSTRAINT user_confirmation_methods_method_check,
    ADD CONSTRAINT user_confirmation_methods_method_check CHECK (method IN ('sms', 'email', 'totp', 'passkey'));

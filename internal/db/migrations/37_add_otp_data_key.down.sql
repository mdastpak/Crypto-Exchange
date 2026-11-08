-- The otp-mac keys protect only sign-in sessions, which expire within
-- otp.auth.ttl_seconds, so they can go. The previous release signs codes
-- with otp.auth.secret again, which must then be set.
DELETE FROM data_keys WHERE purpose = 'otp-mac';
ALTER TABLE data_keys DROP CONSTRAINT data_keys_purpose_check;
ALTER TABLE data_keys ADD CONSTRAINT data_keys_purpose_check
    CHECK (purpose IN ('row-mac', 'field-enc', 'blind-index'));

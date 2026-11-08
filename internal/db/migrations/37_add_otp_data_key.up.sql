-- The otp-mac data keys (#146): the root of the keys that protect sign-in
-- codes and contacts in Redis, which otp.auth.secret held before.
--
-- Rolling out: instances of the previous release load every row of
-- data_keys whatever its purpose, so they keep starting and reloading after
-- an instance of this release creates otp-mac/v1.
ALTER TABLE data_keys DROP CONSTRAINT data_keys_purpose_check;
ALTER TABLE data_keys ADD CONSTRAINT data_keys_purpose_check
    CHECK (purpose IN ('row-mac', 'field-enc', 'blind-index', 'otp-mac'));

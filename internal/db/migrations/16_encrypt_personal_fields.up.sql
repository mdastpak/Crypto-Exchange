-- Personal fields encrypted by the application (#140), first step: the
-- columns of the ciphertext and of the blind index, next to the plaintext.
-- This release writes both, reads the ciphertext first, and finds contacts
-- by their blind index; a job encrypts the rows written before it. So an
-- instance of the previous release, which knows only the plaintext, keeps
-- working during a rolling deploy. The next release stops writing the
-- plaintext and empties it. A sealed value (keys.SealField) and a blind
-- index each start with the decimal version of their key and a colon.

ALTER TABLE user_contacts
    ADD COLUMN value_enc bytea,
    ADD COLUMN value_bidx bytea,
    ALTER COLUMN value DROP NOT NULL,
    ADD CONSTRAINT user_contacts_value_stored CHECK (value IS NOT NULL OR value_enc IS NOT NULL),
    ADD CONSTRAINT user_contacts_value_indexed CHECK ((value_enc IS NULL) = (value_bidx IS NULL));

-- The uniqueness of contacts once the plaintext is gone. While it is not,
-- user_contacts_value_unique keeps holding it too.
CREATE UNIQUE INDEX user_contacts_value_bidx_unique ON user_contacts (type, value_bidx)
    WHERE removed_at IS NULL;

ALTER TABLE users
    ADD COLUMN first_name_enc bytea,
    ADD COLUMN last_name_enc bytea,
    ADD COLUMN display_name_enc bytea,
    ADD COLUMN last_login_ip_enc bytea;

-- A write of a plaintext value that leaves its ciphertext as it was comes
-- from an instance of the previous release. The ciphertext, and the blind
-- index, no longer match the value, and reads prefer them, so they are
-- dropped; the job encrypts the new value again. This code base writes a
-- new ciphertext with every change of the plaintext, even one that empties
-- it: each seal has a nonce of its own, so the ciphertext always changes.
CREATE FUNCTION user_contacts_plaintext_written() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.value IS DISTINCT FROM OLD.value AND NEW.value_enc IS NOT DISTINCT FROM OLD.value_enc THEN
        NEW.value_enc := NULL;
        NEW.value_bidx := NULL;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER user_contacts_plaintext_written BEFORE UPDATE OF value ON user_contacts
    FOR EACH ROW EXECUTE FUNCTION user_contacts_plaintext_written();

CREATE FUNCTION users_plaintext_written() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.first_name IS DISTINCT FROM OLD.first_name AND NEW.first_name_enc IS NOT DISTINCT FROM OLD.first_name_enc THEN
        NEW.first_name_enc := NULL;
    END IF;
    IF NEW.last_name IS DISTINCT FROM OLD.last_name AND NEW.last_name_enc IS NOT DISTINCT FROM OLD.last_name_enc THEN
        NEW.last_name_enc := NULL;
    END IF;
    IF NEW.display_name IS DISTINCT FROM OLD.display_name AND NEW.display_name_enc IS NOT DISTINCT FROM OLD.display_name_enc THEN
        NEW.display_name_enc := NULL;
    END IF;
    IF NEW.last_login_ip IS DISTINCT FROM OLD.last_login_ip AND NEW.last_login_ip_enc IS NOT DISTINCT FROM OLD.last_login_ip_enc THEN
        NEW.last_login_ip_enc := NULL;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER users_plaintext_written BEFORE UPDATE OF first_name, last_name, display_name, last_login_ip ON users
    FOR EACH ROW EXECUTE FUNCTION users_plaintext_written();

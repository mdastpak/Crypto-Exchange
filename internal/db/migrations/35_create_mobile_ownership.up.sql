-- Mobile ownership against the national ID (#253).
--
-- user_national_ids holds the verified national ID of a user, sealed with
-- a field-enc data key and bound to its user (#140), so that the database
-- never holds it in the clear. The identity verification of #158 writes
-- it. The mobile number of a user who has one must belong to it.
CREATE TABLE user_national_ids (
    user_hk            UUID PRIMARY KEY REFERENCES users(hk) ON DELETE CASCADE,
    national_id_sealed BYTEA NOT NULL,
    verified_at        TIMESTAMPTZ NOT NULL,
    -- The provider that verified it, and its reference of the check.
    provider           TEXT NOT NULL CHECK (provider <> ''),
    provider_ref       TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The last successful check that a mobile number belongs to the national
-- ID of its user: when, by which provider, and the provider's reference,
-- for disputes and for the recheck at sign-in (#252).
ALTER TABLE user_contacts
    ADD COLUMN ownership_checked_at TIMESTAMPTZ,
    ADD COLUMN ownership_provider   TEXT,
    ADD COLUMN ownership_ref        TEXT;

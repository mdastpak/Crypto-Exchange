-- The legal documents that a user accepted (#316): the terms of service,
-- the privacy notice, and others that legal.documents requires. A row is
-- one acceptance of one version, with where it came from. Rows are only
-- added: an acceptance is evidence, and a new version adds a new row.
CREATE TABLE user_agreements (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_hk UUID NOT NULL REFERENCES users (hk),
    kind TEXT NOT NULL,
    version TEXT NOT NULL,
    accepted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ip INET,
    user_agent TEXT,
    -- The country of the request, when it is known (#284).
    country TEXT,
    CONSTRAINT user_agreements_kind_check CHECK (kind <> ''),
    CONSTRAINT user_agreements_version_check CHECK (version <> ''),
    CONSTRAINT user_agreements_key UNIQUE (user_hk, kind, version)
);

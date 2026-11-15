-- The balances whose forced conversion failed (#452), by the migration or
-- the delisting that converts them: the claim of the conversions skips a
-- held balance until retry_after, so one balance that cannot convert does
-- not stop the others. The code is a stable name of the error, never its
-- text. A conversion that succeeds releases its hold, with released_at: the
-- runtime role deletes no row of it (#248). A balance held again reopens it.
CREATE TABLE currency_conversion_holds (
    reference_type text NOT NULL CONSTRAINT currency_conversion_holds_reference_type_check
        CHECK (reference_type IN ('currency_migration', 'currency_delisting')),
    reference_id uuid NOT NULL,
    balance_id uuid NOT NULL,
    currency_hk uuid NOT NULL,
    attempts integer NOT NULL DEFAULT 1 CONSTRAINT currency_conversion_holds_attempts_check CHECK (attempts > 0),
    last_error_code text NOT NULL CONSTRAINT currency_conversion_holds_code_check CHECK (last_error_code ~ '^[a-z_]{1,40}$'),
    retry_after timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    released_at timestamptz,
    PRIMARY KEY (reference_type, reference_id, balance_id)
);

CREATE INDEX currency_conversion_holds_open ON currency_conversion_holds (reference_type, reference_id) WHERE released_at IS NULL;

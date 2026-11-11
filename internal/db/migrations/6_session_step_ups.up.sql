-- Step-up of users (#307): a session may run the operations that the second
-- factor protects, such as moving funds, only until elevated_until, which a
-- sign-in with the password and a fresh password check set. It is a table
-- of its own, so that elevating a session writes nothing that the seal of
-- the session covers.
CREATE TABLE session_step_ups (
    session_id UUID PRIMARY KEY REFERENCES sessions (id),
    elevated_until TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

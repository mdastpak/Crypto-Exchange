-- Store the role of each user, so that admin tokens are only issued to users
-- whose stored role is admin, never to a role chosen by the client. Existing
-- users keep the default role, user.
ALTER TABLE users ADD COLUMN role VARCHAR(16) NOT NULL DEFAULT 'user';
ALTER TABLE users ADD CONSTRAINT users_role_check CHECK (role IN ('user', 'admin'));

-- The time step of the last TOTP code accepted for the user. A code is only
-- accepted for a later step, so an observed code cannot be replayed.
ALTER TABLE users ADD COLUMN totp_last_step BIGINT;

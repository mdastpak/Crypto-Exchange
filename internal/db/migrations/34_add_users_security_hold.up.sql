-- Security hold after a security change (#251). When the email address,
-- the mobile number, or the second factor of a user changes, the
-- operations that a second factor protects, such as withdrawals, are held
-- until security_hold_until, so that whoever made the change cannot move
-- funds out at once and the owner has time to react. A change extends the
-- hold and never shortens it. NULL, or a time in the past, means no hold.
ALTER TABLE users ADD COLUMN security_hold_until TIMESTAMPTZ;

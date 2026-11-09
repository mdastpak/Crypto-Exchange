-- The preferences of a user that follow them across devices and domains
-- (#286). locale is a BCP 47 tag of i18n.locales, or NULL for no choice, so
-- that each request negotiates its locale; the server checks the tag, since
-- the supported locales come from the configuration. theme is for the
-- frontends: light, dark, or system, which follows the device.
ALTER TABLE users
    ADD COLUMN locale VARCHAR(35) CHECK (locale ~ '^[A-Za-z]{2,8}(-[A-Za-z0-9]{1,8})*$'),
    ADD COLUMN theme TEXT NOT NULL DEFAULT 'system' CHECK (theme IN ('light', 'dark', 'system'));

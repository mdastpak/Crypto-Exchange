DROP TABLE integrity_findings;
ALTER TABLE sessions DROP COLUMN seal_key_version, DROP COLUMN row_seal, DROP COLUMN row_version;
ALTER TABLE user_contacts DROP COLUMN seal_key_version, DROP COLUMN row_seal, DROP COLUMN row_version;
ALTER TABLE users DROP COLUMN seal_key_version, DROP COLUMN row_seal, DROP COLUMN row_version;

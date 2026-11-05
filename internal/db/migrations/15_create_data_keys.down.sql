-- Dropping the table destroys the data keys. Data sealed or encrypted with
-- them can no longer be verified or read.
DROP TABLE IF EXISTS data_keys;

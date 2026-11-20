CREATE SCHEMA IF NOT EXISTS gateway;
CREATE TABLE IF NOT EXISTS gateway.records (
	record_key bytea PRIMARY KEY CHECK (octet_length(record_key) BETWEEN 1 AND 1024),
	record_value bytea NOT NULL CHECK (octet_length(record_value) <= 1048576)
)

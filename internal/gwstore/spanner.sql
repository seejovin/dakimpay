CREATE TABLE GatewayRecords (
	RecordKey BYTES(1024) NOT NULL,
	RecordValue BYTES(MAX) NOT NULL
) PRIMARY KEY (RecordKey)

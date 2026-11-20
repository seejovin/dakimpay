CREATE SCHEMA IF NOT EXISTS virtualrail;

-- Operational continuity belongs to the independent processor ledger. It is
-- unrelated to the origin, shared HTTP credential, or merchant account name.
CREATE TABLE IF NOT EXISTS virtualrail.ledger_identity (
  singleton boolean PRIMARY KEY CHECK (singleton),
  ledger_id text NOT NULL CHECK (ledger_id ~ '^[a-f0-9]{64}$')
);

CREATE TABLE IF NOT EXISTS virtualrail.program (
  merchant text NOT NULL, account text NOT NULL, currency text NOT NULL,
  PRIMARY KEY (merchant, account),
  UNIQUE (account)
);

CREATE TABLE IF NOT EXISTS virtualrail.terminal (
  id text PRIMARY KEY, merchant text NOT NULL, account text NOT NULL, outlet text NOT NULL,
  credential_digest bytea NOT NULL UNIQUE,
  FOREIGN KEY (merchant, account) REFERENCES virtualrail.program
);

CREATE TABLE IF NOT EXISTS virtualrail.operation (
  merchant text NOT NULL, account text NOT NULL, id text NOT NULL,
  kind text NOT NULL CHECK (kind IN ('charge','refund')),
  parent text NOT NULL, order_id text NOT NULL, outlet text NOT NULL, terminal text NOT NULL,
  channel text NOT NULL CHECK (channel IN ('terminal','hosted')),
  amount bigint NOT NULL CHECK (amount > 0), currency text NOT NULL,
  state text NOT NULL CHECK (state IN ('pending','captured','declined','refunded')),
  movement text NOT NULL DEFAULT '',
  hosted_capability text UNIQUE,
  deadline timestamptz NOT NULL DEFAULT (clock_timestamp() + interval '15 minutes'),
  PRIMARY KEY (merchant, account, id),
  FOREIGN KEY (merchant, account) REFERENCES virtualrail.program,
  CHECK ((state IN ('captured','refunded')) = (movement <> '')),
  CHECK ((kind='refund') = (parent <> ''))
);
CREATE UNIQUE INDEX IF NOT EXISTS virtualrail_lane_in_use
  ON virtualrail.operation(terminal) WHERE channel='terminal' AND state='pending';
CREATE INDEX IF NOT EXISTS virtualrail_parent
  ON virtualrail.operation(merchant, account, parent) WHERE kind='refund';
CREATE TABLE IF NOT EXISTS virtualrail.movement (
  reference text PRIMARY KEY, merchant text NOT NULL, account text NOT NULL, operation_id text NOT NULL,
  kind text NOT NULL CHECK(kind IN ('capture','refund')),
  amount bigint NOT NULL, currency text NOT NULL, recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  UNIQUE(merchant, account, operation_id), FOREIGN KEY(merchant, account, operation_id) REFERENCES virtualrail.operation,
  CHECK ((kind='capture' AND amount>0) OR (kind='refund' AND amount<0))
);

CREATE TABLE IF NOT EXISTS virtualrail.export_program (
  merchant text NOT NULL, account text NOT NULL, beneficiary text NOT NULL,
  credential_digest bytea NOT NULL UNIQUE,
  PRIMARY KEY (merchant, account),
  FOREIGN KEY (merchant,account) REFERENCES virtualrail.program
);
CREATE TABLE IF NOT EXISTS virtualrail.remittance (
  merchant text NOT NULL, account text NOT NULL, id text NOT NULL,
  report bytea NOT NULL, source bytea NOT NULL,
  PRIMARY KEY (merchant, account, id),
  FOREIGN KEY (merchant, account) REFERENCES virtualrail.export_program
);
CREATE TABLE IF NOT EXISTS virtualrail.allocation (
  movement text PRIMARY KEY REFERENCES virtualrail.movement(reference),
  merchant text NOT NULL, account text NOT NULL, remittance text NOT NULL,
  FOREIGN KEY (merchant, account, remittance) REFERENCES virtualrail.remittance
);
CREATE INDEX IF NOT EXISTS virtualrail_movement_for_export ON virtualrail.movement(merchant,account,recorded_at,reference);

-- Legacy originals keep their empty route terms. New routed programs and
-- financial movements preserve the independent provider-account assignment.
CREATE TABLE IF NOT EXISTS virtualrail.route_binding (
  merchant text NOT NULL, account text NOT NULL, route_id text NOT NULL,
  connector_id text NOT NULL, provider_account text NOT NULL UNIQUE,
  PRIMARY KEY (merchant,account),
  FOREIGN KEY (merchant,account) REFERENCES virtualrail.program
);
ALTER TABLE virtualrail.operation
  ADD COLUMN IF NOT EXISTS payment_method text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS route_id text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS connector_id text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS provider_account text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS card_brand text NOT NULL DEFAULT '';

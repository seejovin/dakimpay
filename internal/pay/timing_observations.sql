-- A completed observation covers the processor and bank inventories through
-- the same effective instant. There is deliberately no historical backfill.
CREATE TABLE IF NOT EXISTS pay.timing_observations (
 known_at timestamptz PRIMARY KEY,
 effective_through timestamptz NOT NULL CHECK (effective_through <= known_at),
 merchant_id text NOT NULL DEFAULT 'mimi' CHECK (merchant_id='mimi'),
 currency text NOT NULL DEFAULT 'USD' CHECK (currency='USD')
);

-- An immutable original only needs storing once. Subsequent observations must
-- agree with all the previously accepted facts in their effective interval.
CREATE TABLE IF NOT EXISTS pay.timing_facts (
 source text NOT NULL CHECK (source IN ('processor','bank','booking')),
 bank_trace text NOT NULL,
 operation_id text NOT NULL,
 payout_id text NOT NULL,
 amount bigint NOT NULL,
 effective_at timestamptz NOT NULL,
 first_known_at timestamptz NOT NULL REFERENCES pay.timing_observations(known_at),
 CHECK (effective_at<=first_known_at),
 CHECK (CASE WHEN source='bank' THEN operation_id='' AND payout_id=''
             ELSE operation_id<>'' AND payout_id<>'' END),
 PRIMARY KEY (source,bank_trace)
);
CREATE UNIQUE INDEX IF NOT EXISTS timing_operation_by_source ON pay.timing_facts(source,operation_id) WHERE source<>'bank';
CREATE UNIQUE INDEX IF NOT EXISTS timing_payout_by_source ON pay.timing_facts(source,payout_id) WHERE source<>'bank';
CREATE INDEX IF NOT EXISTS timing_facts_known ON pay.timing_facts(first_known_at,effective_at);

CREATE OR REPLACE FUNCTION pay.keep_timing_evidence() RETURNS trigger LANGUAGE plpgsql AS $immutable$
BEGIN
 RAISE EXCEPTION 'A completed timing observation or its original evidence cannot be revised';
END
$immutable$;
CREATE OR REPLACE TRIGGER keep_timing_observations BEFORE UPDATE OR DELETE OR TRUNCATE ON pay.timing_observations
 FOR EACH STATEMENT EXECUTE FUNCTION pay.keep_timing_evidence();
CREATE OR REPLACE TRIGGER keep_timing_facts BEFORE UPDATE OR DELETE OR TRUNCATE ON pay.timing_facts
 FOR EACH STATEMENT EXECUTE FUNCTION pay.keep_timing_evidence();

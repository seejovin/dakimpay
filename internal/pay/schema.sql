CREATE SCHEMA IF NOT EXISTS pay;
CREATE TABLE IF NOT EXISTS pay.payments (
 id text PRIMARY KEY,
 merchant_id text NOT NULL CHECK (merchant_id='mimi'),
 request_key text NOT NULL,
 order_id text NOT NULL,
 channel text NOT NULL CHECK (channel IN ('terminal','online')),
 amount bigint NOT NULL CHECK (amount BETWEEN 1 AND 100000000),
 currency text NOT NULL CHECK (currency='USD'),
 brand text NOT NULL CHECK (brand IN ('visa','mastercard','amex')),
 terminal_id text NOT NULL,
 lose_response boolean NOT NULL,
 status text NOT NULL DEFAULT 'unknown',
 reference text,
 checkout_url text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CONSTRAINT payment_checkout_status CHECK (status IN ('unknown','awaiting_customer','authorized','captured') OR (channel='online' AND status='expired' AND reference IS NOT NULL)),
 UNIQUE(merchant_id,request_key), UNIQUE(merchant_id,order_id), UNIQUE(merchant_id,reference)
);
CREATE TABLE IF NOT EXISTS pay.refunds (
 id text PRIMARY KEY,
 request_key text NOT NULL UNIQUE,
 payment_id text NOT NULL REFERENCES pay.payments,
 amount bigint NOT NULL CHECK (amount BETWEEN 1 AND 100000000),
 currency text NOT NULL CHECK (currency='USD'),
 status text NOT NULL DEFAULT 'unknown' CHECK (status IN ('unknown','succeeded')),
 reference text UNIQUE,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS refunds_for_payment ON pay.refunds(payment_id);
CREATE TABLE IF NOT EXISTS pay.settlement_requests (
 request_key text PRIMARY KEY, operation_id text NOT NULL UNIQUE, batch text NOT NULL
);
CREATE TABLE IF NOT EXISTS pay.payouts (
 id text PRIMARY KEY, operation_id text NOT NULL UNIQUE REFERENCES pay.settlement_requests(operation_id),
 bank_trace text NOT NULL UNIQUE, currency text NOT NULL CHECK(currency='USD'), net bigint NOT NULL,
 status text NOT NULL CHECK(status IN ('matched','in_transit')),
 bank_evidence jsonb,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE IF NOT EXISTS pay.lines (
 id text PRIMARY KEY, payout_id text NOT NULL REFERENCES pay.payouts,
 kind text NOT NULL CHECK(kind IN ('capture','refund','fee')), reference text NOT NULL,
 amount bigint NOT NULL,
 UNIQUE(kind,reference)
);
CREATE INDEX IF NOT EXISTS payout_lines ON pay.lines(payout_id);
CREATE TABLE IF NOT EXISTS pay.events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, subject text NOT NULL,
 kind text NOT NULL, message text NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(subject,kind)
);
ALTER TABLE pay.settlement_requests ADD COLUMN IF NOT EXISTS empty boolean NOT NULL DEFAULT false;
CREATE TABLE IF NOT EXISTS pay.settlement_keys (
 request_key text PRIMARY KEY, operation_id text NOT NULL REFERENCES pay.settlement_requests(operation_id)
);
CREATE TABLE IF NOT EXISTS pay.settlement_expected (
 operation_id text NOT NULL REFERENCES pay.settlement_requests(operation_id),
 kind text NOT NULL, reference text NOT NULL,
 PRIMARY KEY(operation_id,kind,reference)
);

ALTER TABLE pay.payments
 ADD COLUMN IF NOT EXISTS next_recovery timestamptz NOT NULL DEFAULT (now() + interval '45 seconds'),
 ADD COLUMN IF NOT EXISTS recovery_attempts integer NOT NULL DEFAULT 0;
ALTER TABLE pay.refunds
 ADD COLUMN IF NOT EXISTS next_recovery timestamptz NOT NULL DEFAULT (now() + interval '45 seconds'),
 ADD COLUMN IF NOT EXISTS recovery_attempts integer NOT NULL DEFAULT 0;
-- Existing local installations use the original PostgreSQL-generated check name.
DO $upgrade$
BEGIN
 IF NOT EXISTS(SELECT FROM pg_constraint WHERE conrelid='pay.payments'::regclass AND conname='payment_checkout_status') THEN
  ALTER TABLE pay.payments DROP CONSTRAINT IF EXISTS payments_status_check, DROP CONSTRAINT IF EXISTS payment_evidence_status,
   ADD CONSTRAINT payment_checkout_status CHECK (status IN ('unknown','awaiting_customer','authorized','captured') OR (channel='online' AND status='expired' AND reference IS NOT NULL));
  DROP INDEX IF EXISTS pay.unresolved_payments_due;
  DROP INDEX IF EXISTS pay.unconfirmed_payments_due;
 END IF;
END
$upgrade$;
CREATE INDEX IF NOT EXISTS pending_payments_due ON pay.payments(next_recovery,id) WHERE status IN ('unknown','awaiting_customer','authorized');
CREATE INDEX IF NOT EXISTS unresolved_refunds_due ON pay.refunds(next_recovery,id) WHERE status='unknown';

-- One independent inbox per configured processor identity. Original envelopes
-- are represented by validated fields and a digest over the exact signed bytes.
CREATE TABLE IF NOT EXISTS pay.processor_callbacks (
 source text NOT NULL,
 event_id text NOT NULL,
 merchant_id text NOT NULL CHECK (merchant_id = 'mimi'),
 operation_id text NOT NULL,
 event_type text NOT NULL CONSTRAINT checkout_callback_kind CHECK (event_type IN ('payment.awaiting_customer','payment.expired','payment.captured','refund.succeeded','refund.observed')),
 digest bytea NOT NULL CHECK (octet_length(digest) = 32),
 status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','applied','exception')),
 detail text NOT NULL DEFAULT 'Awaiting original processor evidence.',
 received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 attempts integer NOT NULL DEFAULT 0,
 next_attempt timestamptz NOT NULL DEFAULT now(),
 checked_at timestamptz,
 PRIMARY KEY (source,event_id)
);
CREATE INDEX IF NOT EXISTS processor_callbacks_due ON pay.processor_callbacks(next_attempt,event_id) WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS pay.processor_callback_conflicts (
 source text NOT NULL,
 event_id text NOT NULL,
 digest bytea NOT NULL CHECK (octet_length(digest) = 32),
 received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (source,event_id,digest),
 FOREIGN KEY (source,event_id) REFERENCES pay.processor_callbacks
);

-- An adjustment and an ordinary refund consume the same capture. Source
-- operations cannot collide with client idempotency keys or enter dispatch.
CREATE SEQUENCE IF NOT EXISTS pay.adjustment_sequence;
ALTER TABLE pay.refunds
 ALTER COLUMN request_key DROP NOT NULL,
 ADD COLUMN IF NOT EXISTS source_operation text UNIQUE,
 ADD COLUMN IF NOT EXISTS source_sequence bigint UNIQUE,
 ADD COLUMN IF NOT EXISTS occurred_at timestamptz,
 ADD COLUMN IF NOT EXISTS original_payout text REFERENCES pay.payouts(id);
DO $upgrade$
BEGIN
 IF NOT EXISTS (SELECT FROM pg_constraint WHERE conrelid='pay.processor_callbacks'::regclass AND conname='checkout_callback_kind') THEN
  ALTER TABLE pay.processor_callbacks DROP CONSTRAINT IF EXISTS processor_callbacks_event_type_check, DROP CONSTRAINT IF EXISTS callback_event_kind,
   ADD CONSTRAINT checkout_callback_kind CHECK (event_type IN ('payment.awaiting_customer','payment.expired','payment.captured','refund.succeeded','refund.observed'));
 END IF;
 IF EXISTS (SELECT FROM pg_constraint WHERE conrelid='pay.refunds'::regclass AND conname='refunds_status_check') THEN
  ALTER TABLE pay.refunds DROP CONSTRAINT refunds_status_check, ADD CONSTRAINT refund_outcome_status
   CHECK (status IN ('unknown','succeeded','declined'));
 END IF;
 IF NOT EXISTS (SELECT FROM pg_constraint WHERE conrelid='pay.refunds'::regclass AND conname='refund_provenance') THEN
  ALTER TABLE pay.refunds ADD CONSTRAINT refund_provenance CHECK (
   (request_key IS NOT NULL AND source_operation IS NULL AND source_sequence IS NULL AND occurred_at IS NULL AND original_payout IS NULL)
   OR (request_key IS NULL AND source_operation IS NOT NULL AND source_sequence IS NOT NULL AND source_sequence>0
       AND occurred_at IS NOT NULL AND original_payout IS NOT NULL AND reference IS NOT NULL AND status='succeeded'));
 END IF;
END
$upgrade$;

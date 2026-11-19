package simulator

const acquirerSchema = `
CREATE TABLE IF NOT EXISTS sim_charge (
 operation_id text PRIMARY KEY,
 reference text NOT NULL UNIQUE DEFAULT ('ch_' || gen_random_uuid()::text),
 merchant_id text NOT NULL CHECK(merchant_id='mimi'),
 order_id text NOT NULL,
 channel text NOT NULL CHECK(channel IN ('terminal','online')),
 amount bigint NOT NULL CHECK(amount > 0 AND amount <= 100000000),
 currency text NOT NULL CHECK(currency='USD'),
 brand text NOT NULL CHECK(brand IN ('visa','mastercard','amex')),
 terminal_id text NOT NULL,
 status text NOT NULL CHECK(status IN ('captured','awaiting_customer')),
 checkout_token text UNIQUE,
 created_at timestamptz NOT NULL DEFAULT now(),
 captured_at timestamptz,
 UNIQUE(merchant_id,order_id),
 CHECK((channel='terminal' AND terminal_id='MIMI-01' AND checkout_token IS NULL) OR (channel='online' AND terminal_id='' AND checkout_token IS NOT NULL)),
 CHECK((captured_at IS NOT NULL) = (status='captured'))
);
CREATE TABLE IF NOT EXISTS sim_refund (
 operation_id text PRIMARY KEY,
 reference text NOT NULL UNIQUE DEFAULT ('rf_' || gen_random_uuid()::text),
 charge_reference text NOT NULL REFERENCES sim_charge(reference),
 amount bigint NOT NULL CHECK(amount>0),
 merchant_id text NOT NULL CHECK(merchant_id='mimi'),
 currency text NOT NULL CHECK(currency='USD'),
 created_at timestamptz NOT NULL DEFAULT now()
);
-- Old captures have no recorded dispute observation. Keep them on hold.
ALTER TABLE sim_charge ADD COLUMN IF NOT EXISTS dispute_status text NOT NULL DEFAULT 'unknown'
 CHECK(dispute_status IN ('none','open','unknown'));
-- Existing unpaid hosted sessions receive a full window on this upgrade.
-- A historical capture keeps its original timestamps and reference.
DO $upgrade$
BEGIN
 IF NOT EXISTS(SELECT FROM pg_constraint WHERE conrelid='sim_charge'::regclass AND conname='sim_checkout_deadline') THEN
  ALTER TABLE sim_charge ADD COLUMN checkout_expires_at timestamptz,
   DROP CONSTRAINT sim_charge_status_check,
   ADD CONSTRAINT sim_charge_status_check CHECK(status IN ('captured','awaiting_customer','expired'));
  UPDATE sim_charge SET checkout_expires_at=statement_timestamp()+interval '15 minutes' WHERE status='awaiting_customer';
  ALTER TABLE sim_charge ADD CONSTRAINT sim_checkout_deadline CHECK(
    (channel='terminal' AND status='captured' AND checkout_expires_at IS NULL) OR
    (channel='online' AND (status='captured' OR checkout_expires_at IS NOT NULL)));
 END IF;
END $upgrade$;
CREATE INDEX IF NOT EXISTS sim_due_checkouts ON sim_charge(checkout_expires_at,operation_id) WHERE status='awaiting_customer';
CREATE INDEX IF NOT EXISTS sim_refunds_by_charge ON sim_refund(charge_reference);
-- Every refund in the preceding simulator contract required a merchant command.
ALTER TABLE sim_refund ADD COLUMN IF NOT EXISTS origin text NOT NULL DEFAULT 'merchant'
 CHECK(origin IN ('merchant','acquirer'));
CREATE TABLE IF NOT EXISTS sim_refund_decline (
 operation_id text PRIMARY KEY,
 charge_reference text NOT NULL REFERENCES sim_charge(reference),
 amount bigint NOT NULL CHECK(amount>0),
 merchant_id text NOT NULL CHECK(merchant_id='mimi'),
 currency text NOT NULL CHECK(currency='USD')
);
ALTER TABLE sim_refund_decline ADD COLUMN IF NOT EXISTS origin text NOT NULL DEFAULT 'merchant'
 CHECK(origin IN ('merchant','acquirer'));
CREATE TABLE IF NOT EXISTS sim_payout (
 operation_id text PRIMARY KEY,
 id text NOT NULL UNIQUE DEFAULT ('po_' || gen_random_uuid()::text),
 bank_trace text NOT NULL UNIQUE DEFAULT ('bt_' || gen_random_uuid()::text),
 merchant_id text NOT NULL CHECK(merchant_id='mimi'),
 currency text NOT NULL CHECK(currency='USD'),
 batch text NOT NULL CHECK(batch IN ('standard','guided')),
 net bigint NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sim_payout_timing ON sim_payout(created_at,id);
CREATE TABLE IF NOT EXISTS sim_line (
 sequence bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 id text NOT NULL UNIQUE DEFAULT ('sl_' || gen_random_uuid()::text),
 kind text NOT NULL CHECK(kind IN ('capture','refund','fee')),
 reference text NOT NULL,
 amount bigint NOT NULL,
 batch text NOT NULL CHECK(batch IN ('standard','guided')),
 payout_id text REFERENCES sim_payout(id),
 UNIQUE(kind,reference),
 CHECK((kind='capture' AND amount>0) OR (kind='refund' AND amount<0) OR (kind='fee' AND amount<=0))
);
CREATE INDEX IF NOT EXISTS sim_statement_payout ON sim_line(payout_id,sequence);
-- Keep the capture-time quote independent of the amount ultimately reported
-- on a statement. Older charges never received a quote and are left as-is.
CREATE TABLE IF NOT EXISTS sim_fee_estimate (
 payment_id text PRIMARY KEY REFERENCES sim_charge(operation_id),
 reference text NOT NULL UNIQUE REFERENCES sim_charge(reference),
 merchant_id text NOT NULL CHECK(merchant_id='mimi'),
 amount bigint NOT NULL CHECK(amount BETWEEN 1 AND 100000000),
 currency text NOT NULL CHECK(currency='USD'),
 fee bigint NOT NULL CHECK(fee BETWEEN 0 AND amount),
 rate_bps integer NOT NULL CHECK(rate_bps BETWEEN 0 AND 10000),
 policy text NOT NULL CHECK(policy <> ''),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE OR REPLACE FUNCTION sim_keep_fee_estimate() RETURNS trigger LANGUAGE plpgsql AS $immutable$
BEGIN
 RAISE EXCEPTION 'A recorded processor fee estimate is immutable';
END
$immutable$;
CREATE OR REPLACE TRIGGER sim_keep_fee_estimate
 BEFORE UPDATE OR DELETE OR TRUNCATE ON sim_fee_estimate
 FOR EACH STATEMENT EXECUTE FUNCTION sim_keep_fee_estimate();
CREATE TABLE IF NOT EXISTS sim_callback_outbox (
 event_id text PRIMARY KEY,
 merchant_id text NOT NULL CHECK(merchant_id='mimi'),
 operation_id text NOT NULL,
 kind text NOT NULL CONSTRAINT sim_callback_event_kind CHECK(kind IN ('payment.awaiting_customer','payment.captured','payment.expired','refund.succeeded','refund.observed')),
 body bytea NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 attempts bigint NOT NULL DEFAULT 0,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 last_attempt_at timestamptz,
 last_outcome text,
 acknowledged_at timestamptz,
 UNIQUE(merchant_id,operation_id,kind)
);
CREATE INDEX IF NOT EXISTS sim_due_callbacks ON sim_callback_outbox(next_attempt_at,created_at,event_id)
 WHERE acknowledged_at IS NULL;
DO $upgrade$
BEGIN
 IF EXISTS(SELECT FROM pg_constraint WHERE conrelid='sim_callback_outbox'::regclass AND
    (conname='sim_callback_outbox_kind_check' OR (conname='sim_callback_event_kind' AND position('payment.expired' IN pg_get_constraintdef(oid))=0))) THEN
  ALTER TABLE sim_callback_outbox DROP CONSTRAINT IF EXISTS sim_callback_outbox_kind_check,
   DROP CONSTRAINT IF EXISTS sim_callback_event_kind,
   ADD CONSTRAINT sim_callback_event_kind CHECK(kind IN ('payment.awaiting_customer','payment.captured','payment.expired','refund.succeeded','refund.observed'));
 END IF;
END $upgrade$;
`

const bankSchema = `
CREATE TABLE IF NOT EXISTS sim_bank_entry (
 trace text PRIMARY KEY,
 merchant_id text NOT NULL CHECK(merchant_id='mimi'),
 currency text NOT NULL CHECK(currency='USD'),
 amount bigint NOT NULL,
 beneficiary text NOT NULL DEFAULT 'MIMI-USD-OPERATING' CHECK(beneficiary='MIMI-USD-OPERATING'),
 received_at timestamptz NOT NULL DEFAULT now(),
 posted_at timestamptz
);
CREATE INDEX IF NOT EXISTS sim_bank_posting_timing ON sim_bank_entry(posted_at,trace) WHERE posted_at IS NOT NULL;
`

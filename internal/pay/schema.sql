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
 status text NOT NULL DEFAULT 'unknown' CHECK (status IN ('unknown','awaiting_customer','captured')),
 reference text,
 checkout_url text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
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
CREATE INDEX IF NOT EXISTS unresolved_payments_due ON pay.payments(next_recovery,id) WHERE status IN ('unknown','awaiting_customer');
CREATE INDEX IF NOT EXISTS unresolved_refunds_due ON pay.refunds(next_recovery,id) WHERE status='unknown';

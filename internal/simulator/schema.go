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
CREATE INDEX IF NOT EXISTS sim_refunds_by_charge ON sim_refund(charge_reference);
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
`

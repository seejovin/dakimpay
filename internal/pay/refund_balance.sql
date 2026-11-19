-- Serialize the first baseline and installation across service processes.
SELECT pg_advisory_xact_lock(hashtext('pay:refund_balance_schema'));

-- A provider observation may arrive before the merchant's original refund
-- instructions can be resolved. Retain its source identity without publishing
-- a second refund or spending any more of this capture.
CREATE TABLE IF NOT EXISTS pay.refund_source_holds (
 source_operation text PRIMARY KEY,
 reference text NOT NULL UNIQUE,
 payment_id text NOT NULL REFERENCES pay.payments,
 amount bigint NOT NULL CHECK (amount BETWEEN 1 AND 100000000),
 currency text NOT NULL CHECK (currency='USD'),
 charge_reference text NOT NULL,
 original_payout text NOT NULL REFERENCES pay.payouts,
 occurred_at timestamptz NOT NULL,
 next_check timestamptz NOT NULL DEFAULT now(),
 last_message text NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS refund_source_holds_payment ON pay.refund_source_holds(payment_id);
CREATE INDEX IF NOT EXISTS refund_source_holds_due ON pay.refund_source_holds(next_check,source_operation);

-- Take the legacy baseline once, in the same transaction as table creation.
-- Repeated startups preserve its original timestamp and avoid reclassifying
-- a later discrepancy as historical. The view below still reports new ones.
DO $upgrade$
BEGIN
 IF to_regclass('pay.refund_legacy_reviews') IS NULL THEN
   CREATE TABLE pay.refund_legacy_reviews (
    payment_id text PRIMARY KEY REFERENCES pay.payments,
    captured_at_detection bigint NOT NULL,
    succeeded_at_detection bigint NOT NULL,
    pending_at_detection bigint NOT NULL,
    detected_at timestamptz NOT NULL DEFAULT now()
   );
   INSERT INTO pay.refund_legacy_reviews(payment_id,captured_at_detection,succeeded_at_detection,pending_at_detection)
    SELECT p.id,p.amount,coalesce(sum(r.amount) FILTER(WHERE r.status='succeeded'),0),
           coalesce(sum(r.amount) FILTER(WHERE r.status='unknown'),0)
    FROM pay.payments p JOIN pay.refunds r ON r.payment_id=p.id
    WHERE p.status='captured' AND r.status IN ('unknown','succeeded')
    GROUP BY p.id,p.amount HAVING sum(r.amount)>p.amount;
 END IF;
END
$upgrade$;

-- No ordering of uncertain merchant instructions can establish which one the
-- acquirer accepted. Show their nominal total separately from the amount of
-- capture still held. A pending source observation also freezes new spending.
CREATE OR REPLACE VIEW pay.refund_balances AS
 SELECT p.id AS payment_id,p.currency,r.succeeded,r.pending_requested,
        least(r.pending_requested,b.remaining) AS reserved,
        CASE WHEN h.held THEN 0 ELSE greatest(0::bigint,b.remaining-r.pending_requested) END AS available,
        (h.held OR r.succeeded+r.pending_requested>c.captured) AS review
 FROM pay.payments p
 CROSS JOIN LATERAL (
   SELECT coalesce(sum(amount) FILTER(WHERE status='succeeded'),0)::bigint AS succeeded,
          coalesce(sum(amount) FILTER(WHERE status='unknown'),0)::bigint AS pending_requested
   FROM pay.refunds WHERE payment_id=p.id
 ) r
 CROSS JOIN LATERAL (SELECT CASE WHEN p.status='captured' THEN p.amount ELSE 0::bigint END AS captured) c
 CROSS JOIN LATERAL (SELECT greatest(0::bigint,c.captured-r.succeeded) AS remaining) b
 CROSS JOIN LATERAL (SELECT EXISTS(SELECT FROM pay.refund_source_holds WHERE payment_id=p.id) AS held) h;

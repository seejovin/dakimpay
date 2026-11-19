package pay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A presentation deadline or delivery of a signed expiry hint never supplies
// proof of closure. Use distinct opaque provider references for the two final
// outcomes and require every inspection to be read-only.
func TestCheckoutDeadlineRequiresOriginalProviderOutcome(t *testing.T) {
	for _, final := range []string{"captured", "expired"} {
		t.Run(final, func(t *testing.T) {
			ctx := context.Background()
			db := isolatedPool(t)
			var source atomic.Value
			source.Store("awaiting_customer")
			var reads atomic.Int32
			deadline := time.Now().Add(-time.Minute)
			const id = "dp-original"
			const ref = "provider-original-091"
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/sim/charges/"+id || r.Header.Get("Authorization") != "Bearer private-fixture" {
					t.Errorf("unexpected command %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				reads.Add(1)
				status := source.Load().(string)
				if status == "missing" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				answer(w, 200, chargeFact{OperationID: id, OrderID: "original-order", MerchantID: "mimi",
					Reference: ref, Channel: "online", Brand: "visa", Amount: 750, Currency: "USD",
					Status: status, CheckoutURL: "/checkout/test", CheckoutExpiresAt: &deadline})
			}))
			defer provider.Close()
			start := func() *Service {
				t.Helper()
				s, err := New(ctx, db, provider.URL, provider.URL, "private-fixture")
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			service := start()
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := db.Exec(ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`INSERT INTO pay.payments(id,merchant_id,request_key,order_id,channel,amount,currency,brand,terminal_id,lose_response,status,reference,checkout_url)
			     VALUES ($1,'mimi','same-attempt','original-order','online',750,'USD','visa','',false,'awaiting_customer',$2,$3)`, id, ref, provider.URL+"/checkout/test")
			work := func() {
				t.Helper()
				if err := service.ProcessCallbacks(ctx); err != nil {
					t.Fatal(err)
				}
			}
			deliver := func(eventID, kind string, want int) {
				t.Helper()
				got := callbackRequest(service.CallbackHandler(signingFixture), envelope(t, eventID, id, kind), signingFixture, time.Now())
				if got != want {
					t.Fatalf("delivery %s = %d, want %d", eventID, got, want)
				}
			}
			expect := func(want, sql string, args ...any) {
				t.Helper()
				var got string
				if err := db.QueryRow(ctx, sql, args...).Scan(&got); err != nil || got != want {
					t.Fatalf("got %q want %q: %v (%s)", got, want, err, sql)
				}
			}
			const eventStatus = "SELECT status FROM pay.processor_callbacks WHERE event_id=$1"
			original := PaymentInput{OrderID: "original-order", Channel: "online", Amount: 750, Currency: "USD", Brand: "visa"}
			noReplacement := func() {
				t.Helper()
				code, res := issue(service.Handler(), "POST", "/v1/payments", "replacement", original)
				if code != 409 {
					t.Fatalf("replacement returned %d %v", code, res)
				}
			}
			deliver("expiry", "payment.expired", 202)
			work()
			expect("pending", eventStatus, "expiry")
			expect("awaiting_customer", "SELECT status FROM pay.payments WHERE id=$1", id)
			noReplacement()
			// A bound provider record disappearing, even after the deadline,
			// leaves the original checkout unresolved and sends no command.
			source.Store("missing")
			code, record := issue(service.Handler(), "POST", "/v1/payments/"+id+"/recover", "", nil)
			if code != 200 || record["status"] != "awaiting_customer" || record["reference"] != ref {
				t.Fatal("missing source record", code, record)
			}
			noReplacement()

			source.Store(final)
			service = start()
			exec("UPDATE pay.processor_callbacks SET next_attempt=now() WHERE event_id='expiry'")
			work()
			deliver("expiry", "payment.expired", 200)
			deliver("late-presentation", "payment.awaiting_customer", 202)
			work()
			expect("applied", eventStatus, "expiry")
			expect("applied", eventStatus, "late-presentation")
			code, record = issue(service.Handler(), "POST", "/v1/payments", "same-attempt", original)
			if code != 200 || record["id"] != id || record["reference"] != ref || record["checkout_url"] != "" || record["status"] != final {
				t.Fatal("verified result", code, record)
			}
			noReplacement()
			expect("1", "SELECT count(*)::text FROM pay.events WHERE subject=$1 AND kind=$2", id, final)
			before := reads.Load()
			exec("UPDATE pay.payments SET next_recovery=now()")
			if err := service.recoverDue(ctx); err != nil || reads.Load() != before {
				t.Fatal("worker reclaimed final payment", err, before, reads.Load())
			}

			// Inconsistent final evidence must stay visible without silently
			// changing the amount eligible for release or reopening checkout.
			if final == "captured" {
				source.Store("expired")
			} else {
				deliver("unbacked-capture", "payment.captured", 202)
				work()
				expect("exception", eventStatus, "unbacked-capture")
				source.Store("captured")
			}
			deliver("conflict", "payment.captured", 202)
			work()
			expect("exception", eventStatus, "conflict")
			expect(final, "SELECT status FROM pay.payments WHERE id=$1", id)
			expect("1", "SELECT count(*)::text FROM pay.events WHERE subject=$1 AND kind IN ('captured','expired')", id)
		})
	}
}

// Exercise the immediately preceding installation's named constraints, keeping
// a pre-existing checkout, provider reference, and pending notification intact.
func TestCheckoutExpiryUpgradePreservesPendingAttempt(t *testing.T) {
	ctx := context.Background()
	db := isolatedPool(t)
	start := func() {
		t.Helper()
		if _, err := New(ctx, db, "http://provider.invalid", "http://bank.invalid", "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	start()
	_, err := db.Exec(ctx, `ALTER TABLE pay.payments DROP CONSTRAINT payment_checkout_status,
	 ADD CONSTRAINT payment_evidence_status CHECK(status IN ('unknown','awaiting_customer','authorized','captured'));
	 DROP INDEX pay.pending_payments_due;
	 CREATE INDEX unconfirmed_payments_due ON pay.payments(next_recovery,id) WHERE status<>'captured';
	 ALTER TABLE pay.processor_callbacks DROP CONSTRAINT checkout_callback_kind,
	 ADD CONSTRAINT callback_event_kind CHECK(event_type IN ('payment.awaiting_customer','payment.captured','refund.succeeded','refund.observed'));
	 INSERT INTO pay.payments(id,merchant_id,request_key,order_id,channel,amount,currency,brand,terminal_id,lose_response,status,reference,checkout_url)
	 VALUES ('old-payment','mimi','old-key','old-order','online',50,'USD','visa','',false,'awaiting_customer','old-reference','/checkout/old');
	 INSERT INTO pay.processor_callbacks(source,event_id,merchant_id,operation_id,event_type,digest)
	 VALUES ('simulated-acquirer','old-event','mimi','old-payment','payment.awaiting_customer',decode(repeat('00',32),'hex'))`)
	if err != nil {
		t.Fatal(err)
	}
	start()
	start()
	p, err := payment(ctx, db, "old-payment")
	if err != nil || p.Reference != "old-reference" || p.Status != "awaiting_customer" || p.CheckoutURL != "/checkout/old" {
		t.Fatal("original attempt after migration", p, err)
	}
	var pending int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM pay.processor_callbacks WHERE event_id='old-event' AND status='pending'").Scan(&pending); err != nil || pending != 1 {
		t.Fatal("pending notification after migration", pending, err)
	}
	_, err = db.Exec(ctx, `UPDATE pay.payments SET status='expired',checkout_url='' WHERE id='old-payment';
	 INSERT INTO pay.processor_callbacks(source,event_id,merchant_id,operation_id,event_type,digest)
	 VALUES ('simulated-acquirer','new-expiry-event','mimi','old-payment','payment.expired',decode(repeat('00',32),'hex'))`)
	if err != nil {
		t.Fatal(err)
	}
}

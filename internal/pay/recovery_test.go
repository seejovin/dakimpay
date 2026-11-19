package pay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"dakimpay/internal/simulator"
)

// Advance only the durable due timestamps. Finance and payment outcomes must
// come from the independent acquirer and the same worker used by the service.
func TestBackgroundRecoveryAfterClosedTabAndServiceRestart(t *testing.T) {
	ctx := context.Background()
	db := isolatedPool(t)
	aqDB := isolatedPool(t)
	aq := httptest.NewUnstartedServer(nil)
	url := "http://" + aq.Listener.Addr().String()
	acquirer, err := simulator.AcquirerWithKeys(ctx, aqDB, url, "http://127.0.0.1:1", "service", "instruct", "approve")
	if err != nil {
		t.Fatal(err)
	}
	aq.Config.Handler = acquirer
	aq.Start()
	defer aq.Close()
	start := func() *Service {
		t.Helper()
		s, err := New(ctx, db, aq.URL, "http://127.0.0.1:1", "service")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := start()
	h := s.Handler()
	create := func(in PaymentInput, want int) obj {
		t.Helper()
		status, p := issue(h, "POST", "/v1/payments", in.OrderID, in)
		if status != want {
			t.Fatalf("%d %v", status, p)
		}
		return p
	}
	web := create(PaymentInput{"WEB", "online", 1500, "USD", "visa", "", false}, 201)
	abandoned := create(PaymentInput{"ABANDONED", "online", 900, "USD", "mastercard", "", false}, 201)
	till := create(PaymentInput{"TILL", "terminal", 1200, "USD", "amex", "MIMI-01", true}, 202)
	// Browser leaves immediately after the partner accepted the test card.
	req, err := http.NewRequest("POST", web["checkout_url"].(string), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", aq.URL)
	resp, err := aq.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("hosted checkout %d", resp.StatusCode)
	}
	// Leave the refund persisted before simulation of an interrupted service.
	refundSale := create(PaymentInput{"REFUND-SALE", "terminal", 700, "USD", "mastercard", "MIMI-01", false}, 201)
	if _, err = db.Exec(ctx, `INSERT INTO pay.refunds(id,request_key,payment_id,amount,currency) VALUES ('dr_recover','RF', $1,250,'USD')`, refundSale["id"]); err != nil {
		t.Fatal(err)
	}
	s = start()
	count := func(want int, sql string, args ...any) {
		t.Helper()
		var n int
		if err := db.QueryRow(ctx, sql, args...).Scan(&n); err != nil || n != want {
			t.Fatalf("%s: got %d want %d: %v", sql, n, want, err)
		}
	}
	if err = s.recoverDue(ctx); err != nil {
		t.Fatal(err)
	}
	count(0, "SELECT count(*) FROM pay.payments WHERE recovery_attempts>0")
	count(0, "SELECT count(*) FROM pay.refunds WHERE recovery_attempts>0")
	advance := func() {
		t.Helper()
		if _, err := db.Exec(ctx, "UPDATE pay.payments SET next_recovery=now(); UPDATE pay.refunds SET next_recovery=now()"); err != nil {
			t.Fatal(err)
		}
	}
	advance()
	if err = s.recoverDue(ctx); err != nil {
		t.Fatal(err)
	}
	count(2, "SELECT count(*) FROM pay.payments WHERE id IN ($1,$2) AND status='captured' AND recovery_attempts=1", web["id"], till["id"])
	count(1, "SELECT count(*) FROM pay.refunds WHERE id='dr_recover' AND status='succeeded' AND recovery_attempts=1")
	count(1, "SELECT count(*) FROM pay.payments WHERE id=$1 AND status='awaiting_customer' AND recovery_attempts=1 AND next_recovery > now()+interval '40 seconds'", abandoned["id"])
	if err = s.recoverDue(ctx); err != nil {
		t.Fatal(err)
	} // A fresh pass cannot reclaim the abandoned checkout early.
	count(1, "SELECT count(*) FROM pay.payments WHERE id=$1 AND recovery_attempts=1", abandoned["id"])
	advance()
	if err = s.recoverDue(ctx); err != nil {
		t.Fatal(err)
	}
	count(1, "SELECT count(*) FROM pay.payments WHERE id=$1 AND recovery_attempts=2 AND next_recovery>now()+interval '80 seconds'", abandoned["id"])
	count(0, "SELECT count(*) FROM pay.payments WHERE status='captured' AND recovery_attempts>1")
	count(0, "SELECT count(*) FROM pay.refunds WHERE status='succeeded' AND recovery_attempts>1")
	var charges, refunds int
	if err = aqDB.QueryRow(ctx, "SELECT (SELECT count(*) FROM sim_charge),(SELECT count(*) FROM sim_refund)").Scan(&charges, &refunds); err != nil || charges != 4 || refunds != 1 {
		t.Fatalf("acquirer operations %d + %d %v", charges, refunds, err)
	}
}

package pay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// An authenticated provider fixture can report authorization before capture.
// Exercise the merchant entry points and overlap a delayed lookup with the
// scheduled recovery worker while the shopper's browser is closed.
func TestAuthorizationWaitsForOriginalCapture(t *testing.T) {
	db := isolatedPool(t)
	ctx := context.Background()
	var state atomic.Value
	state.Store("awaiting_customer")
	var dispatches atomic.Int32
	var pauseNextRead atomic.Bool
	reading, release := make(chan struct{}), make(chan struct{})
	var resume sync.Once
	unblock := func() { resume.Do(func() { close(release) }) }
	const reference = "AUTH-CHARGE"
	aq := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-proof" {
			t.Error("processor inspection missing service identity")
			w.WriteHeader(401)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/sim/payouts" {
			// The statement claims a capture even before local verification.
			answer(w, 200, Payout{ID: "PAYOUT-AUTH", MerchantID: "mimi", Currency: "USD", BankTrace: "BANK-AUTH", Net: 4095, Lines: []Line{
				{"LINE-AUTH", "capture", reference, 4200}, {"FEE-AUTH", "fee", reference, -105},
			}})
			return
		}
		fact := chargeFact{Reference: reference, MerchantID: "mimi", OrderID: "AUTH-ORDER", Channel: "online", Amount: 4200, Currency: "USD", Brand: "visa", CheckoutURL: "/checkout/auth"}
		status := state.Load().(string)
		switch {
		case r.Method == "POST" && r.URL.Path == "/sim/charges":
			dispatches.Add(1)
			var original struct {
				ID string `json:"operation_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&original); err != nil {
				t.Error(err)
			}
			fact.OperationID = original.ID
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/sim/charges/"):
			fact.OperationID = strings.TrimPrefix(r.URL.Path, "/sim/charges/")
			if pauseNextRead.Swap(false) {
				close(reading)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
		default:
			t.Errorf("unexpected processor command %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		switch status {
		case "missing":
			w.WriteHeader(404)
			return
		case "wrong-amount":
			fact.Amount--
			status = "authorized"
		case "replaced":
			fact.Reference = "ANOTHER-REFERENCE"
			status = "captured"
		}
		fact.Status = status
		answer(w, 200, fact)
	}))
	defer aq.Close()
	defer unblock()
	s, err := New(ctx, db, aq.URL, aq.URL, "local-proof")
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	call := func(method, path, key string, body any, want int) obj {
		t.Helper()
		code, result := issue(h, method, path, key, body)
		if code != want {
			t.Fatalf("%s %s = %d, want %d: %v", method, path, code, want, result)
		}
		return result
	}
	input := PaymentInput{OrderID: "AUTH-ORDER", Channel: "online", Amount: 4200, Currency: "USD", Brand: "visa"}
	p := call("POST", "/v1/payments", "auth-sale", input, 201)
	if p["status"] != "awaiting_customer" || p["checkout_url"] != aq.URL+"/checkout/auth" {
		t.Fatal("initial checkout", p)
	}
	id := p["id"].(string)
	path := "/v1/payments/" + id
	check := func(status string) {
		t.Helper()
		row := call("GET", path, "", nil, 200)
		if row["status"] != status || row["reference"] != reference || row["checkout_url"] != "" {
			t.Fatalf("expected %s and retired checkout: %v", status, row)
		}
	}
	state.Store("wrong-amount")
	call("POST", path+"/recover", "", nil, 502)
	if p := call("GET", path, "", nil, 200); p["status"] != "awaiting_customer" {
		t.Fatal(p)
	}
	state.Store("authorized")
	call("POST", path+"/recover", "", nil, 200)
	check("authorized")
	state.Store("awaiting_customer")
	call("POST", path+"/recover", "", nil, 200)
	check("authorized")
	state.Store("missing")
	call("POST", path+"/recover", "", nil, 200)
	check("authorized")
	state.Store("replaced")
	call("POST", path+"/recover", "", nil, 502)
	check("authorized")
	original := call("POST", "/v1/payments", "auth-sale", input, 200)
	if original["id"] != id || original["status"] != "authorized" {
		t.Fatal(original)
	}
	call("POST", "/v1/payments", "replacement-sale", input, 409)
	call("POST", "/v1/settle", "auth-statement", nil, 409)
	state.Store("authorized")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, p := issue(h, "POST", path+"/recover", "", nil); code != 200 || p["status"] != "authorized" {
				t.Errorf("repeated authorization %d: %v", code, p)
			}
			if code, p := issue(h, "POST", path+"/refunds", "auth-refund", obj{"amount": 100}); code != 409 {
				t.Errorf("uncaptured refund %d: %v", code, p)
			}
		}()
	}
	wg.Wait()
	d := call("GET", "/v1/dashboard", "", nil, 200)
	if len(d["payouts"].([]any)) != 0 || len(d["refunds"].([]any)) != 0 {
		t.Fatal("unverified financial records", d)
	}
	for metric, v := range d["totals"].(map[string]any) {
		if v != float64(0) {
			t.Errorf("%s posted before capture: %v", metric, v)
		}
	}

	// Hold an authorized response. Advance only the original processor record to
	// captured, then let the scheduled worker verify it before releasing the old read.
	pauseNextRead.Store(true)
	delayed := make(chan obj, 1)
	go func() { _, result := issue(h, "POST", path+"/recover", "", nil); delayed <- result }()
	select {
	case <-reading:
	case <-time.After(3 * time.Second):
		t.Fatal("old read did not reach processor")
	}
	state.Store("captured")
	if _, err := db.Exec(ctx, "UPDATE pay.payments SET next_recovery=now() WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverDue(ctx); err != nil {
		t.Fatal(err)
	}
	check("captured")
	unblock()
	if p := <-delayed; p["status"] != "captured" {
		t.Fatal("late authorization regressed capture", p)
	}
	if dispatches.Load() != 1 {
		t.Fatalf("expected original command only, got %d", dispatches.Load())
	}
	var a, c int
	if err := db.QueryRow(ctx, "SELECT count(*) FILTER (WHERE kind='authorized'), count(*) FILTER (WHERE kind='captured') FROM pay.events WHERE subject=$1", id).Scan(&a, &c); err != nil || a != 1 || c != 1 {
		t.Fatal("authorization/capture transition counts", a, c, err)
	}
	payout := call("POST", "/v1/settle", "auth-statement", nil, 201)
	if payout["net"] != float64(4095) || payout["status"] != "in_transit" {
		t.Fatal(payout)
	}
	d = call("GET", "/v1/dashboard", "", nil, 200)
	totals := d["totals"].(map[string]any)
	if totals["captured"] != float64(4200) || totals["fees"] != float64(105) || totals["pending_deposit"] != float64(4095) || totals["deposited"] != float64(0) {
		t.Fatal("verified capture totals", totals)
	}
}

// Older installations named their payment-status check automatically.
func TestAuthorizationStatusUpgradePreservesOriginalPayment(t *testing.T) {
	db := isolatedPool(t)
	ctx := context.Background()
	start := func() {
		t.Helper()
		if _, err := New(ctx, db, "http://processor.invalid", "http://bank.invalid", "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	start()
	_, err := db.Exec(ctx, `ALTER TABLE pay.payments DROP CONSTRAINT payment_checkout_status,
	 ADD CONSTRAINT payments_status_check CHECK(status IN ('unknown','awaiting_customer','captured'));
	 DROP INDEX pay.pending_payments_due;
	 CREATE INDEX unresolved_payments_due ON pay.payments(next_recovery,id) WHERE status IN ('unknown','awaiting_customer');
	 INSERT INTO pay.payments(id,merchant_id,request_key,order_id,channel,amount,currency,brand,terminal_id,lose_response)
	 VALUES('upgrade-order','mimi','upgrade-key','order-1','terminal',100,'USD','visa','MIMI-01',false)`)
	if err != nil {
		t.Fatal(err)
	}
	start()
	start()
	tag, err := db.Exec(ctx, `UPDATE pay.payments SET status='authorized', reference='existing-authorization' WHERE id='upgrade-order' AND status='unknown' AND amount=100`)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatal("upgrade lost original payment or blocked authorization", tag, err)
	}
}

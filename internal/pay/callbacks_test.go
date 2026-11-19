package pay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dakimpay/internal/processor"
)

const signingFixture = "local-only-callback-test-key"

func callbackRequest(h http.Handler, raw []byte, key string, now time.Time) int {
	r := httptest.NewRequest("POST", "/callbacks/processor", bytes.NewReader(raw))
	processor.Sign(r.Header, raw, key, now)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func envelope(t *testing.T, id, op, kind string) []byte {
	t.Helper()
	body, err := json.Marshal(processor.Event{ID: id, MerchantID: "mimi", OperationID: op, Type: kind})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// Persist an ambiguous operation before any response is available to dakim pay.
// The processor stub implements only inspection. Any attempted charge creation,
// presentation or refund from a notification fails the test.
func TestConcurrentCallbackDeliveryAndRestart(t *testing.T) {
	db := isolatedPool(t)
	ctx := context.Background()
	var reads atomic.Int64
	processorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/sim/charges/dp_till" || r.Header.Get("Authorization") != "Bearer service" {
			t.Errorf("unexpected processor request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		reads.Add(1)
		answer(w, 200, chargeFact{OperationID: "dp_till", Reference: "acq1", MerchantID: "mimi", Status: "captured", OrderID: "order1", Channel: "terminal", Amount: 4200, Currency: "USD", Brand: "visa", TerminalID: "MIMI-01"})
	}))
	defer processorServer.Close()
	start := func() *Service {
		s, err := New(ctx, db, processorServer.URL, processorServer.URL, "service")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := start()
	_, err := db.Exec(ctx, `INSERT INTO pay.payments(id,merchant_id,request_key,order_id,channel,amount,currency,brand,terminal_id,lose_response)
	VALUES('dp_till','mimi','till-key','order1','terminal',4200,'USD','visa','MIMI-01',true)`)
	if err != nil {
		t.Fatal(err)
	}
	h := s.CallbackHandler(signingFixture)
	raw := envelope(t, "event-1", "dp_till", "payment.captured")
	now := time.Now()
	var malformed []byte
	malformed = append(malformed, raw[:len(raw)-1]...)
	malformed = append(malformed, []byte(`,"type":"refund.succeeded"}`)...)
	for _, bad := range []struct {
		raw  []byte
		key  string
		at   time.Time
		want int
	}{
		{raw, "wrong-test-key", now, 401},
		{raw, signingFixture, now.Add(-24 * time.Hour), 401},
		{malformed, signingFixture, now, 400},
		{envelope(t, "event-invalid", "dp_till", "other.event"), signingFixture, now, 422},
		{bytes.Replace(raw, []byte("mimi"), []byte("another"), 1), signingFixture, now, 422},
		{bytes.Repeat([]byte("x"), 8200), signingFixture, now, 413},
	} {
		if got := callbackRequest(h, bad.raw, bad.key, bad.at); got != bad.want {
			t.Fatalf("invalid event: got %d want %d", got, bad.want)
		}
	}
	count := func(want int, query string, args ...any) {
		t.Helper()
		var got int
		if err := db.QueryRow(ctx, query, args...).Scan(&got); err != nil || got != want {
			t.Fatalf("count %d, want %d: %v (%s)", got, want, err, query)
		}
	}
	count(0, "SELECT count(*) FROM pay.processor_callbacks")

	const deliveries = 12
	var wg sync.WaitGroup
	results := make(chan int, deliveries)
	barrier := make(chan struct{})
	for range deliveries {
		wg.Add(1)
		go func() { defer wg.Done(); <-barrier; results <- callbackRequest(h, raw, signingFixture, now) }()
	}
	close(barrier)
	wg.Wait()
	close(results)
	response := map[int]int{}
	for code := range results {
		response[code]++
	}
	if response[202] != 1 || response[200] != deliveries-1 {
		t.Fatal("concurrent delivery receipts", response)
	}
	count(1, "SELECT count(*) FROM pay.processor_callbacks WHERE status='pending' AND attempts=0")
	if reads.Load() != 0 {
		t.Fatal("receipt performed I/O before durable handoff")
	}

	s = start()
	h = s.CallbackHandler(signingFixture)
	// Two workers and the merchant's in-flight status request can inspect the same
	// capture. All must converge on a single posting of the original evidence.
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.ProcessCallbacks(ctx) }()
	}
	code, data := issue(s.Handler(), "POST", "/v1/payments/dp_till/recover", "", nil)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if code != 200 || data["status"] != "captured" {
		t.Fatalf("recover: %d %v", code, data)
	}
	count(1, "SELECT count(*) FROM pay.events WHERE subject='dp_till' AND kind='captured'")
	count(1, "SELECT count(*) FROM pay.processor_callbacks WHERE status='applied' AND attempts=1")
	verifyReads := reads.Load()
	change := envelope(t, "event-1", "unknown-other", "payment.captured")
	for range 2 {
		if got := callbackRequest(h, change, signingFixture, now); got != 409 {
			t.Fatal("payload change", got)
		}
	}
	if got := callbackRequest(h, raw, signingFixture, now); got != 200 {
		t.Fatal("duplicate after completion", got)
	}
	if err := s.ProcessCallbacks(ctx); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != verifyReads {
		t.Fatal("duplicate re-queued an applied event")
	}
	count(1, "SELECT count(*) FROM pay.processor_callback_conflicts")
	code, dash := issue(s.Handler(), "GET", "/v1/dashboard", "", nil)
	if code != 200 {
		t.Fatal(code, dash)
	}
	records := dash["callbacks"].([]any)
	first := records[0].(map[string]any)
	if len(records) != 1 || first["operation_id"] != "dp_till" || first["attempts"] != float64(1) || first["conflicts"] != float64(1) || first["status"] != "applied" {
		t.Fatal("callback history", records)
	}
	if got := dash["totals"].(map[string]any)["captured"]; got != float64(4200) {
		t.Fatalf("posted tender %v", got)
	}
}

func TestOutOfOrderCallbackEvidenceAndRefundBinding(t *testing.T) {
	db := isolatedPool(t)
	ctx := context.Background()
	status := "awaiting_customer"
	refundMode := "missing"
	var processorServer *httptest.Server
	processorServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer lookup" {
			t.Errorf("callback issued financial command: %s", r.Method)
			w.WriteHeader(405)
			return
		}
		switch r.URL.Path {
		case "/sim/charges/dp_web":
			answer(w, 200, chargeFact{OperationID: "dp_web", Reference: "capture-web", MerchantID: "mimi", Status: status, OrderID: "order-web", Channel: "online", Amount: 6800, Currency: "USD", Brand: "amex", CheckoutURL: processorServer.URL + "/checkout/example"})
		case "/sim/charges/dp_wrong":
			answer(w, 200, chargeFact{OperationID: "dp_wrong", Reference: "wrong", MerchantID: "mimi", Status: "captured", OrderID: "order-wrong", Channel: "terminal", Amount: 100, Currency: "USD", Brand: "visa", TerminalID: "MIMI-01"})
		case "/sim/refunds/dr_web", "/sim/refunds/dr_other":
			if refundMode == "missing" {
				w.WriteHeader(404)
				return
			}
			op, original := "dr_web", "capture-web"
			if r.URL.Path == "/sim/refunds/dr_other" {
				op, original = "dr_other", "different-original"
			}
			answer(w, 200, obj{"operation_id": op, "merchant_id": "mimi", "amount": 1000, "currency": "USD", "charge_reference": original, "status": "succeeded", "reference": "refund-" + op})
		default:
			t.Errorf("unmatched callback triggered provider lookup: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer processorServer.Close()
	s, err := New(ctx, db, processorServer.URL, processorServer.URL, "lookup")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO pay.payments(id,merchant_id,request_key,order_id,channel,amount,currency,brand,terminal_id,lose_response) VALUES
	('dp_web','mimi','web-key','order-web','online',6800,'USD','amex','',false),
	('dp_wrong','mimi','wrong-key','order-wrong','terminal',200,'USD','visa','MIMI-01',false)`)
	h := s.CallbackHandler(signingFixture)
	send := func(id, op, kind string) {
		t.Helper()
		if code := callbackRequest(h, envelope(t, id, op, kind), signingFixture, time.Now()); code != 202 {
			t.Fatal(code)
		}
	}
	work := func() {
		t.Helper()
		if err := s.ProcessCallbacks(ctx); err != nil {
			t.Fatal(err)
		}
	}
	advance := func(id string) { exec("UPDATE pay.processor_callbacks SET next_attempt=now() WHERE event_id=$1", id) }
	expect := func(want, sql string, args ...any) {
		t.Helper()
		var got string
		if err := db.QueryRow(ctx, sql, args...).Scan(&got); err != nil || got != want {
			t.Fatalf("got %q want %q: %v (%s)", got, want, err, sql)
		}
	}
	const eventState = "SELECT status FROM pay.processor_callbacks WHERE event_id=$1"
	const tenderState = "SELECT status FROM pay.payments WHERE id='dp_web'"
	send("later-capture", "dp_web", "payment.captured")
	work()
	expect("pending", eventState, "later-capture")
	expect("awaiting_customer", tenderState)
	status = "captured"
	advance("later-capture")
	work()
	expect("applied", eventState, "later-capture")
	expect("captured", tenderState)
	send("delayed-presentation", "dp_web", "payment.awaiting_customer")
	status = "awaiting_customer"
	work()
	expect("captured", tenderState)
	expect("pending", eventState, "delayed-presentation")
	status = "captured"
	advance("delayed-presentation")
	work()
	expect("applied", eventState, "delayed-presentation")
	expect("Earlier checkout notification superseded by verified capture.", "SELECT detail FROM pay.processor_callbacks WHERE event_id='delayed-presentation'")
	send("wrong-value", "dp_wrong", "payment.captured")
	send("unmatched", "dp_unissued", "payment.captured")
	work()
	expect("exception", eventState, "wrong-value")
	expect("exception", eventState, "unmatched")
	expect("unknown", "SELECT status FROM pay.payments WHERE id='dp_wrong'")
	exec("INSERT INTO pay.refunds(id,request_key,payment_id,amount,currency) VALUES ('dr_web','R1','dp_web',1000,'USD'),('dr_other','R2','dp_web',1000,'USD')")
	send("refund", "dr_web", "refund.succeeded")
	work()
	expect("pending", eventState, "refund")
	expect("unknown", "SELECT status FROM pay.refunds WHERE id='dr_web'")
	refundMode = "confirmed"
	advance("refund")
	send("wrong-parent", "dr_other", "refund.succeeded")
	work()
	expect("applied", eventState, "refund")
	expect("exception", eventState, "wrong-parent")
	expect("succeeded", "SELECT status FROM pay.refunds WHERE id='dr_web'")
	expect("unknown", "SELECT status FROM pay.refunds WHERE id='dr_other'")
	p, err := payment(ctx, db, "dp_web")
	if err != nil || p.Refunded != 1000 || p.Status != "captured" {
		t.Fatalf("original tender after events: %+v %v", p, err)
	}
}

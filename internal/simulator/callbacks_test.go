package simulator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dakimpay/internal/processor"
)

const callbackTestKey = "synthetic-callback-key"

func TestCallbackOutboxLostAckAndOutOfOrderDelivery(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	web := chargeRequest{Operation: "dp_web", Order: "WEB-47", Merchant: merchant, Currency: currency, Brand: "visa", Channel: "online", Amount: 1700}
	var c charge
	call(t, "POST", s.acquirer.URL+"/sim/charges", serviceKey, web, 200, &c)
	call(t, "POST", s.acquirer.URL+"/sim/charges", serviceKey, web, 200, nil)

	type delivery struct {
		raw     []byte
		headers http.Header
	}
	observed := make(chan delivery, 5)
	var requests atomic.Int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid body", 400)
			return
		}
		observed <- delivery{raw, r.Header.Clone()}
		if requests.Add(1) == 1 {
			panic(http.ErrAbortHandler)
		}
		var e processor.Event
		if err = json.Unmarshal(raw, &e); err != nil {
			http.Error(w, "invalid event", 400)
			return
		}
		jsonResponse(w, http.StatusAccepted, map[string]string{"event_id": e.ID})
	}))
	defer receiver.Close()
	clock := time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC)
	deliver := func(wantErr bool, kind string) delivery {
		t.Helper()
		claimed, err := deliverCallback(ctx, s.aq, receiver.URL, callbackTestKey, func() time.Time { return clock })
		if !claimed || (err != nil) != wantErr {
			t.Fatalf("deliver %s: claimed %v, err %v", kind, claimed, err)
		}
		d := <-observed
		if err = processor.Verify(d.headers, d.raw, callbackTestKey, clock); err != nil {
			t.Fatal(err)
		}
		var e processor.Event
		if err = json.Unmarshal(d.raw, &e); err != nil || e.MerchantID != merchant || e.Type != kind || e.ID == "" {
			t.Fatalf("unexpected event %s, %v", d.raw, err)
		}
		fields := map[string]string{}
		if err = json.Unmarshal(d.raw, &fields); err != nil || len(fields) != 4 {
			t.Fatalf("unexpected callback payload %s", d.raw)
		}
		return d
	}
	first := deliver(true, "payment.awaiting_customer")
	count(t, s.aq, "select count(*) from sim_callback_outbox where attempts=1 and acknowledged_at is null and next_attempt_at>now() and last_outcome='transport unavailable'", 1)

	// Simulate a receiver outage delaying notice of the pending checkout until
	// after its capture and refund reach a restarted sender.
	if _, err := s.aq.Exec(ctx, "update sim_callback_outbox set next_attempt_at=now()+interval '1 day' where kind='payment.awaiting_customer'"); err != nil {
		t.Fatal(err)
	}
	call(t, "POST", s.acquirer.URL+"/sim/approve/"+web.Operation, approveKey, nil, 200, nil)
	call(t, "POST", s.acquirer.URL+"/sim/approve/"+web.Operation, approveKey, nil, 200, nil)
	ref := refundRequest{Operation: "dr_web", Charge: c.Reference, Merchant: merchant, Currency: currency, Amount: 1700}
	var confirmed, inspected refund
	call(t, "POST", s.acquirer.URL+"/sim/refunds", serviceKey, ref, 200, &confirmed)
	call(t, "POST", s.acquirer.URL+"/sim/refunds", serviceKey, ref, 200, nil)
	call(t, "GET", s.acquirer.URL+"/sim/refunds/"+ref.Operation, serviceKey, nil, 200, &inspected)
	if confirmed != inspected {
		t.Fatal("read-only lookup changed original refund evidence")
	}
	call(t, "GET", s.acquirer.URL+"/sim/refunds/missing", serviceKey, nil, 404, nil)
	call(t, "GET", s.acquirer.URL+"/sim/refunds/"+ref.Operation, approveKey, nil, 401, nil)
	clock = clock.Add(10 * time.Minute)
	deliver(false, "payment.captured")
	deliver(false, "refund.succeeded")
	if _, err := s.aq.Exec(ctx, "update sim_callback_outbox set next_attempt_at=now() where kind='payment.awaiting_customer'"); err != nil {
		t.Fatal(err)
	}
	last := deliver(false, "payment.awaiting_customer")
	if !bytes.Equal(first.raw, last.raw) || processor.Verify(first.headers, last.raw, callbackTestKey, clock) == nil {
		t.Fatal("retry changed the event or retained an expired timestamp")
	}
	count(t, s.aq, "select count(*) from sim_callback_outbox where acknowledged_at is not null", 3)
	count(t, s.aq, "select sum(attempts) from sim_callback_outbox", 4)
	count(t, s.aq, "select count(*) from sim_charge where status='captured'", 1)
	count(t, s.aq, "select count(*) from sim_line", 3)
	if busy, err := DeliverCallback(ctx, s.aq, receiver.URL, callbackTestKey); busy || err != nil {
		t.Fatal("acknowledged work resent", busy, err)
	}
}

func TestCallbackQueueRollsBackFinancialWrite(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	if _, err := s.aq.Exec(ctx, "alter table sim_callback_outbox add constraint reject_test_event check(operation_id<>'op_rollback')"); err != nil {
		t.Fatal(err)
	}
	req := chargeRequest{Operation: "op_rollback", Order: "rollback-test", Merchant: merchant, Currency: currency, Brand: "visa", Channel: "terminal", Terminal: "MIMI-01", Amount: 450}
	call(t, "POST", s.acquirer.URL+"/sim/charges", serviceKey, req, 500, nil)
	count(t, s.aq, "select count(*) from sim_line", 0)
	count(t, s.aq, "select count(*) from sim_charge", 0)
	count(t, s.aq, "select count(*) from sim_callback_outbox", 0)
	if _, err := s.aq.Exec(ctx, "alter table sim_callback_outbox drop constraint reject_test_event"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	codes := make(chan int, 12)
	for range cap(codes) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _, _ := request("POST", s.acquirer.URL+"/sim/charges", serviceKey, req)
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 {
			t.Fatal(code)
		}
	}
	count(t, s.aq, "select count(*) from sim_charge", 1)
	count(t, s.aq, "select count(*) from sim_line", 2)
	count(t, s.aq, "select count(*) from sim_callback_outbox where kind='payment.captured'", 1)
}

func TestCallbackLeaseFencesLateFailureAndRejectsRedirect(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	req := chargeRequest{Operation: "dp_fenced", Order: "fencing-test", Merchant: merchant, Currency: currency, Brand: "visa", Channel: "terminal", Terminal: "MIMI-01", Amount: 420}
	call(t, "POST", s.acquirer.URL+"/sim/charges", serviceKey, req, 200, nil)
	var redirected atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Store(true); w.WriteHeader(202) }))
	defer other.Close()
	var attempts atomic.Int32
	blocked := make(chan struct{})
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch attempts.Add(1) {
		case 1:
			http.Redirect(w, r, other.URL, 307)
		case 2:
			close(blocked)
			<-release
			w.WriteHeader(500)
		default:
			var e processor.Event
			if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
				http.Error(w, "invalid event", 400)
				return
			}
			jsonResponse(w, http.StatusAccepted, map[string]string{"event_id": e.ID})
		}
	}))
	defer receiver.Close()
	defer unblock()
	if sent, err := DeliverCallback(ctx, s.aq, receiver.URL, callbackTestKey); !sent || err == nil || redirected.Load() {
		t.Fatalf("redirect not rejected: %v %v", sent, err)
	}
	expire := func() {
		t.Helper()
		if _, err := s.aq.Exec(ctx, "update sim_callback_outbox set next_attempt_at=now()"); err != nil {
			t.Fatal(err)
		}
	}
	expire()
	type result struct {
		sent bool
		err  error
	}
	first := make(chan result, 1)
	go func() {
		sent, err := DeliverCallback(ctx, s.aq, receiver.URL, callbackTestKey)
		first <- result{sent, err}
	}()
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("sender never claimed event")
	}
	if sent, err := DeliverCallback(ctx, s.aq, receiver.URL, callbackTestKey); sent || err != nil {
		t.Fatal("stole a live lease", sent, err)
	}
	expire()
	if sent, err := DeliverCallback(ctx, s.aq, receiver.URL, callbackTestKey); !sent || err != nil {
		t.Fatal("failed to recover expired lease", sent, err)
	}
	unblock()
	select {
	case r := <-first:
		if !r.sent || r.err == nil {
			t.Fatal(r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("original worker did not finish")
	}
	count(t, s.aq, "select count(*) from sim_callback_outbox where attempts=3 and acknowledged_at is not null and last_outcome='HTTP 202'", 1)
}

func TestCallbackRequiresMatchingBoundedReceipt(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	req := chargeRequest{Operation: "dp_receipt", Order: "receipt-test", Merchant: merchant, Currency: currency, Brand: "visa", Channel: "terminal", Terminal: "MIMI-01", Amount: 950}
	call(t, "POST", s.acquirer.URL+"/sim/charges", serviceKey, req, 200, nil)
	var id string
	var original []byte
	if err := s.aq.QueryRow(ctx, "select event_id,body from sim_callback_outbox").Scan(&id, &original); err != nil {
		t.Fatal(err)
	}
	valid := fmt.Sprintf(`{"event_id":%q}`, id)
	type reply struct {
		name   string
		status int
		body   string
	}
	fixtures := []reply{
		{"empty", 202, ""},
		{"fallback page", 200, "<html>Welcome</html>"},
		{"wrong ID", 202, `{"event_id":"evt_other"}`},
		{"ambiguous ID", 200, fmt.Sprintf(`{"event_id":"evt_other","event_id":%q}`, id)},
		{"oversized", 200, valid + strings.Repeat(" ", 4096)},
		{"trailing document", 202, valid + "{}"},
		{"no content", 204, ""},
		{"other success status", 201, valid},
		{"service error", 503, valid},
		{"matching receipt", 200, valid + "\n"},
	}
	replies := make(chan reply, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(raw, original) {
			http.Error(w, "changed event", 400)
			return
		}
		response := <-replies
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.status)
		if response.body != "" {
			_, _ = io.WriteString(w, response.body)
		}
	}))
	defer receiver.Close()
	for i, f := range fixtures {
		replies <- f
		if _, err := s.aq.Exec(ctx, "update sim_callback_outbox set next_attempt_at=now() where acknowledged_at is null"); err != nil {
			t.Fatal(err)
		}
		claimed, err := DeliverCallback(ctx, s.aq, receiver.URL, callbackTestKey)
		wantAck := i == len(fixtures)-1
		if !claimed || (err == nil) != wantAck {
			t.Fatalf("%s: claimed=%v error=%v", f.name, claimed, err)
		}
		var attempts int
		var queued, acked bool
		if err := s.aq.QueryRow(ctx, "select attempts,next_attempt_at>now(),acknowledged_at is not null from sim_callback_outbox where event_id=$1", id).Scan(&attempts, &queued, &acked); err != nil {
			t.Fatal(err)
		}
		if attempts != i+1 || acked != wantAck || (!acked && !queued) {
			t.Fatalf("%s: attempts %d, queued %v, acked %v", f.name, attempts, queued, acked)
		}
	}
	count(t, s.aq, "select count(*) from sim_charge", 1)
	count(t, s.aq, "select count(*) from sim_line", 2)
}

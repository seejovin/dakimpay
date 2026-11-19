package pay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The synthetic processor records one outcome per original operation. It
// withholds replies to one successful refund and one definitive refusal until
// the test enables inquiry. Neither decision is shared with the payment store.
func TestAggregateRefundReservationsAcrossDistinctOperations(t *testing.T) {
	ctx := t.Context()
	db := isolatedPool(t)
	const captured = 1000
	const ref = "CAPTURE-4832"
	const key = "reservation-test"
	var mu sync.Mutex
	source := map[string]refundFact{}
	var delayedSuccess, delayedRefusal string
	submissions := map[string]int{}
	inquiries := map[string]int{}
	inquiryEnabled := false
	corruptCurrency := false
	changeTransport := func(enabled, wrongCurrency bool) {
		mu.Lock()
		defer mu.Unlock()
		inquiryEnabled, corruptCurrency = enabled, wrongCurrency
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("missing provider credential")
			w.WriteHeader(403)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/sim/charges" {
			var fact chargeFact
			if err := json.NewDecoder(r.Body).Decode(&fact); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			fact.Reference, fact.Status = ref, "captured"
			answer(w, 200, fact)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		var op string
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sim/refunds":
			var in refundRequest
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Currency != "USD" || in.ChargeReference != ref || in.MerchantID != "mimi" {
				t.Errorf("unexpected refund instruction: %+v (%v)", in, err)
				w.WriteHeader(422)
				return
			}
			op = in.OperationID
			submissions[op]++
			if old, ok := source[op]; ok {
				if old.refundRequest != in {
					t.Errorf("reused operation with changed terms: %v, originally %v", in, old)
					w.WriteHeader(409)
					return
				}
			} else {
				fact := refundFact{refundRequest: in, Status: "succeeded", Reference: "RF-" + op}
				switch {
				case in.Amount == 401 && delayedSuccess == "":
					delayedSuccess = op
				case in.Amount == 265 && delayedRefusal == "":
					delayedRefusal = op
					fact.Status, fact.Reference = "declined", ""
				}
				source[op] = fact
			}
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/sim/refunds/"):
			op = strings.TrimPrefix(r.URL.Path, "/sim/refunds/")
			inquiries[op]++
		default:
			t.Errorf("unexpected provider request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		fact, exists := source[op]
		if !exists {
			w.WriteHeader(404)
			return
		}
		if op == delayedSuccess || op == delayedRefusal {
			if !inquiryEnabled {
				w.WriteHeader(http.StatusGatewayTimeout)
				return
			}
			if op == delayedRefusal && r.Method == http.MethodPost {
				w.WriteHeader(409)
				return
			}
			if corruptCurrency {
				fact.Currency = "EUR"
			}
		}
		answer(w, 200, fact)
	}))
	defer provider.Close()
	start := func() http.Handler {
		t.Helper()
		s, err := New(ctx, db, provider.URL, provider.URL, key)
		if err != nil {
			t.Fatal(err)
		}
		return s.Handler()
	}
	h := start()
	call := func(method, path, idempotencyKey string, body any, want int) obj {
		t.Helper()
		status, row := issue(h, method, path, idempotencyKey, body)
		if status != want {
			t.Fatalf("%s %s (%s) = %d, want %d: %v", method, path, idempotencyKey, status, want, row)
		}
		return row
	}
	sale := call("POST", "/v1/payments", "sale", PaymentInput{"RESERVE-1", "terminal", captured, "USD", "visa", "MIMI-01", false}, 201)
	id := sale["id"].(string)
	path := "/v1/payments/" + id + "/refunds"
	submit := func(k string, amount, want int) obj {
		t.Helper()
		return call("POST", path, k, obj{"amount": amount}, want)
	}
	check := func(success, pending, refused int64, count int) {
		t.Helper()
		var paid, held, released int64
		var n int
		if err := db.QueryRow(ctx, `SELECT COALESCE(sum(amount) FILTER(WHERE status='succeeded'),0),
			COALESCE(sum(amount) FILTER(WHERE status='unknown'),0), COALESCE(sum(amount) FILTER(WHERE status='declined'),0),count(*)
			FROM pay.refunds WHERE payment_id=$1`, id).Scan(&paid, &held, &released, &n); err != nil {
			t.Fatal(err)
		}
		if paid != success || held != pending || released != refused || n != count || paid+held > captured {
			t.Fatalf("confirmed=%d reserved=%d declined=%d count=%d; want %d/%d/%d/%d", paid, held, released, n, success, pending, refused, count)
		}
		row := call("GET", "/v1/payments/"+id, "", nil, 200)
		if row["refunded"] != float64(success) || row["refund_reserved"] != float64(pending) ||
			row["refundable"] != float64(captured-success-pending) {
			t.Fatalf("merchant projection: refunded=%v reserved=%v refundable=%v; want %d/%d/%d",
				row["refunded"], row["refund_reserved"], row["refundable"], success, pending, captured-success-pending)
		}
	}
	submit("completed", 333, 201)
	pendingA := submit("delayed-success", 401, 202)["id"].(string)
	pendingB := submit("delayed-refusal", 265, 202)["id"].(string)
	if pendingA == pendingB {
		t.Fatal("two reservations share one operation")
	}
	check(333, 666, 0, 3)
	h = start()
	if row := submit("delayed-refusal", 265, 200); row["id"] != pendingB || row["status"] != "unknown" {
		t.Fatal("lost refusal replay changed the original reservation", row)
	}
	submit("delayed-refusal", 266, 409)
	submit("too-much", 2, 409)
	// Each caller wants the same final cent under a different key. Only one
	// may reserve and dispatch it even while both earlier replies are missing.
	const racers = 8
	ready := make(chan struct{})
	codes := make(chan int, racers)
	for i := range racers {
		go func() {
			<-ready
			code, _ := issue(h, "POST", path, fmt.Sprintf("last-cent-%d", i), obj{"amount": 1})
			codes <- code
		}()
	}
	close(ready)
	results := map[int]int{}
	for range racers {
		results[<-codes]++
	}
	if results[201] != 1 || results[409] != racers-1 {
		t.Fatal("final-cent admission", results)
	}
	check(334, 666, 0, 4)
	recoverA := "/v1/refunds/" + pendingA + "/recover"
	recoverB := "/v1/refunds/" + pendingB + "/recover"
	changeTransport(true, true)
	call("POST", recoverB, "", nil, 502)
	call("POST", recoverA, "", nil, 502)
	check(334, 666, 0, 4)
	submit("currency-is-not-closure", 1, 409)
	changeTransport(true, false)
	if row := call("POST", recoverB, "", nil, 200); row["id"] != pendingB || row["status"] != "declined" {
		t.Fatal("incorrect definitive refusal", row)
	}
	check(334, 401, 265, 4)
	if row := submit("delayed-refusal", 265, 200); row["id"] != pendingB || row["status"] != "declined" {
		t.Fatal("refusal replay dispatched replacement", row)
	}
	submit("one-past-released", 266, 409)
	retry := submit("new-authorized-retry", 265, 201)
	if retry["id"] == pendingB || retry["status"] != "succeeded" {
		t.Fatal("explicit retry did not use the released amount", retry)
	}
	check(599, 401, 265, 5)
	submit("cannot-spend-pending-success", 1, 409)
	if row := call("POST", recoverA, "", nil, 200); row["id"] != pendingA || row["status"] != "succeeded" {
		t.Fatal("delayed success did not match original", row)
	}
	h = start()
	call("POST", recoverA, "", nil, 200)
	call("POST", recoverB, "", nil, 200)
	check(captured, 0, 265, 5)
	submit("over-after-restart", 1, 409)
	mu.Lock()
	defer mu.Unlock()
	var refunded int64
	for _, fact := range source {
		if fact.Status == "succeeded" {
			refunded += fact.Amount
		}
	}
	if submissions[pendingA] != 1 || submissions[pendingB] != 1 || inquiries[pendingA] == 0 || inquiries[pendingB] == 0 {
		t.Fatalf("uncertain operations must recover by inquiry: submissions %v, inquiries %v", submissions, inquiries)
	}
	if len(source) != 5 || refunded != captured {
		t.Fatalf("processor recorded %d operations and %d succeeded cents", len(source), refunded)
	}
}

// Each processor commits its actual disposition before sending the first reply.
// Hold its authoritative lookup offline, then return ambiguous JSON, then allow
// the original fact through. The same request key must survive all three steps.
func TestRefundFinalDispositionRequiresUnambiguousEvidence(t *testing.T) {
	tests := []struct {
		name, outcome string
		commandCode   int
	}{
		{"structured_decline_needs_inquiry", "declined", 200},
		{"conflict_needs_inquiry", "declined", 409},
		{"duplicate_success_fields_need_inquiry", "succeeded", 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			db := isolatedPool(t)
			const amount = 57
			const captureRef = "ORIGINAL-REF"
			var mu sync.Mutex
			var stored refundFact
			var submits, reads int
			readMode := "unavailable"
			setMode := func(mode string) {
				mu.Lock()
				defer mu.Unlock()
				readMode = mode
			}
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer proof" {
					w.WriteHeader(403)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == "POST" && r.URL.Path == "/sim/refunds":
					var in refundRequest
					if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Amount != amount ||
						in.Currency != "USD" || in.MerchantID != "mimi" || in.ChargeReference != captureRef {
						t.Errorf("unexpected refund instruction: %+v (%v)", in, err)
						w.WriteHeader(400)
						return
					}
					submits++
					if stored.OperationID != "" && in != stored.refundRequest {
						t.Errorf("created replacement after ambiguous evidence: %+v", in)
						w.WriteHeader(409)
						return
					}
					stored = refundFact{refundRequest: in, Status: tt.outcome}
					if tt.outcome == "succeeded" {
						stored.Reference = "BOUND-ORIGINAL-REFUND"
					}
					if tt.commandCode == 409 {
						w.WriteHeader(409)
						return
					}
				case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/sim/refunds/"):
					if stored.OperationID == "" || r.URL.Path != "/sim/refunds/"+stored.OperationID {
						w.WriteHeader(404)
						return
					}
					reads++
					if readMode == "unavailable" {
						w.WriteHeader(503)
						return
					}
				default:
					t.Errorf("unexpected provider request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(400)
					return
				}
				// The standard decoder would accept only the last currency. A
				// response containing both currencies is ambiguous evidence.
				if (r.Method == "POST" && tt.outcome == "succeeded") || (r.Method == "GET" && readMode == "malformed") {
					raw, err := json.Marshal(stored)
					if err != nil {
						t.Error(err)
						return
					}
					_, _ = w.Write(append([]byte(`{"currency":"EUR",`), raw[1:]...))
					return
				}
				answer(w, 200, stored)
			}))
			defer provider.Close()
			newService := func() http.Handler {
				t.Helper()
				s, err := New(ctx, db, provider.URL, provider.URL, "proof")
				if err != nil {
					t.Fatal(err)
				}
				return s.Handler()
			}
			h := newService()
			// This fixture starts at a durably verified synthetic capture. All
			// refund commands and reads below go through the merchant handler.
			_, err := db.Exec(ctx, `INSERT INTO pay.payments
				(id,merchant_id,request_key,order_id,channel,amount,currency,brand,terminal_id,lose_response,status,reference)
				VALUES('dp-original','mimi','capture-key','merchant-order','terminal',$1,'USD','visa','MIMI-01',false,'captured',$2)`,
				amount, captureRef)
			if err != nil {
				t.Fatal(err)
			}
			const path = "/v1/payments/dp-original/refunds"
			code, _ := issue(h, "POST", path, "original", obj{"amount": amount})
			if code != 202 && code != 502 {
				t.Fatalf("unverified command result returned %d", code)
			}
			held := func() string {
				t.Helper()
				status, row := issue(h, "POST", path, "original", obj{"amount": amount})
				if status != 200 || row["status"] != "unknown" {
					t.Fatal("original reservation lost", status, row)
				}
				status, out := issue(h, "POST", path, "extra", obj{"amount": 1})
				if status != 409 {
					t.Fatal("unverified evidence freed the reservation", status, out)
				}
				return row["id"].(string)
			}
			original := held()
			h = newService()
			recover := "/v1/refunds/" + original + "/recover"
			setMode("malformed")
			code, row := issue(h, "POST", recover, "", nil)
			if code != 502 && !(code == 200 && row["status"] == "unknown") {
				t.Fatalf("malformed read treated as a final outcome: %d %v", code, row)
			}
			if id := held(); id != original {
				t.Fatal("changed identity after restart", id, original)
			}
			setMode("final")
			code, row = issue(h, "POST", recover, "", nil)
			if code != 200 || row["id"] != original || row["status"] != tt.outcome {
				t.Fatal("valid original inquiry did not resolve reservation", code, row)
			}
			code, row = issue(h, "POST", path, "original", obj{"amount": amount})
			if code != 200 || row["id"] != original || row["status"] != tt.outcome {
				t.Fatal("final outcome replay changed the original operation", code, row)
			}
			mu.Lock()
			defer mu.Unlock()
			if submits != 1 || reads < 2 {
				t.Fatalf("dispatches=%d inquiries=%d; expected one dispatch and original lookups only", submits, reads)
			}
		})
	}
}

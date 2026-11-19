package pay

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dakimpay/internal/processor"
	"dakimpay/internal/simulator"
)

func TestMerchantReversalEvidenceReadsBothLedgers(t *testing.T) {
	ctx := t.Context()
	payDB, acquirerDB := isolatedPool(t), isolatedPool(t)
	acq := httptest.NewUnstartedServer(nil)
	addr := "http://" + acq.Listener.Addr().String()
	handler, err := simulator.AcquirerWithKeys(ctx, acquirerDB, addr, addr, "sim-processor", "sim-instruct", "sim-buyer")
	if err != nil {
		t.Fatal(err)
	}
	var sourcePosts atomic.Int32
	var pauseNext atomic.Bool
	var malformed atomic.Int32
	sourceRead, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	acq.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			sourcePosts.Add(1)
		}
		if variant := malformed.Load(); variant != 0 && strings.HasSuffix(r.URL.Path, "/reversal-evidence") {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			raw := bytes.TrimSpace(rec.Body.Bytes())
			switch variant {
			case 1:
				raw = append(raw, []byte(` {"complete":false}`)...)
			case 2:
				raw = append(raw, bytes.Repeat([]byte(" "), 4<<20)...)
			case 3:
				raw = raw[:len(raw)-1]
			case 4:
				raw = bytes.Replace(raw, []byte(`"dispute_status":"none"`), []byte(`"dispute_status":"open","dispute_status":"none"`), 1)
			case 5:
				raw = bytes.Replace(raw, []byte(`"amount":720`), []byte(`"amount":1,"amount":720`), 1)
			case 6:
				raw = bytes.Replace(raw, []byte(`"dispute_status":"none"`), []byte(`"Dispute_Status":"none"`), 1)
			}
			_, _ = w.Write(raw)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/reversal-evidence") && pauseNext.CompareAndSwap(true, false) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			close(sourceRead)
			select {
			case <-release:
				w.WriteHeader(rec.Code)
				_, _ = w.Write(rec.Body.Bytes())
			case <-r.Context().Done():
			}
			return
		}
		handler.ServeHTTP(w, r)
	})
	acq.Start()
	defer acq.Close()
	defer unblock()
	s, err := New(ctx, payDB, addr, addr, "sim-processor")
	if err != nil {
		t.Fatal(err)
	}
	api := s.Handler()
	call := func(method, path, key string, body any, want int) obj {
		t.Helper()
		code, out := issue(api, method, path, key, body)
		if code != want {
			t.Fatalf("%s %s returned %d, want %d: %v", method, path, code, want, out)
		}
		return out
	}
	charge := call("POST", "/v1/payments", "tender", PaymentInput{"MIMI-ORDER", "terminal", 720, "USD", "amex", "MIMI-01", false}, 201)
	id := charge["id"].(string)
	base := "/v1/payments/" + id
	path := base + "/reversal-evidence"
	call("GET", "/v1/payments/missing/reversal-evidence", "", nil, 404)
	call("POST", path, "", nil, 405)
	unauthorized, err := http.Get(addr + "/sim/charges/" + id + "/reversal-evidence")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != 401 {
		t.Fatalf("source without credentials: %d", unauthorized.StatusCode)
	}
	check := func(n int, total int64, dispute string) merchantReversals {
		t.Helper()
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		var e merchantReversals
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &e) != nil || !e.Complete || e.Refunds == nil ||
			len(e.Refunds) != n || e.Payment.ID != id || e.Payment.Refunded != total || e.DisputeStatus != dispute {
			t.Fatalf("unexpected itemized evidence: %d %s", rec.Code, rec.Body.String())
		}
		return e
	}
	check(0, 0, "none")
	for _, variant := range []int32{1, 2, 3, 4, 5, 6} {
		malformed.Store(variant)
		call("GET", path, "", nil, 502)
	}
	malformed.Store(0)

	// The acquirer observed zero refunds. Before its reply reaches dakim pay,
	// a concurrent refund completes. Serving the clean snapshot would lose it.
	pauseNext.Store(true)
	result := make(chan int, 1)
	go func() {
		status, _ := issue(api, "GET", path, "", nil)
		result <- status
	}()
	select {
	case <-sourceRead:
	case <-time.After(5 * time.Second):
		t.Fatal("source evidence request never reached the observation point")
	}
	first := call("POST", base+"/refunds", "first", obj{"amount": 260}, 201)
	unblock()
	if status := <-result; status != 409 {
		t.Fatalf("stale acquirer snapshot returned %d", status)
	}
	part := check(1, 260, "none").Refunds[0]
	if part.ID != first["id"] || part.PaymentID != id || part.Reference != first["reference"] ||
		part.ChargeReference != charge["reference"] || part.MerchantOperation != "first" || part.Amount != 260 {
		t.Fatalf("partial refund binding: %+v", part)
	}
	second := call("POST", base+"/refunds", "remainder", obj{"amount": 460}, 201)
	check(2, 720, "none")
	for _, dispute := range []string{"open", "unknown"} {
		if _, err := acquirerDB.Exec(ctx, "UPDATE sim_charge SET dispute_status=$1 WHERE operation_id=$2", dispute, id); err != nil {
			t.Fatal(err)
		}
		check(2, 720, dispute)
	}
	// A committed source refund with its acknowledgement still outstanding on
	// the merchant ledger must remain in review even if both amounts add up.
	if _, err := payDB.Exec(ctx, "UPDATE pay.refunds SET status='unknown',reference=NULL WHERE id=$1", second["id"]); err != nil {
		t.Fatal(err)
	}
	call("GET", path, "", nil, 409)
	call("GET", path, "", nil, 409)
	if got := sourcePosts.Load(); got != 3 {
		t.Fatalf("evidence read issued a financial instruction: unexpected %d POSTs", got)
	}

	// Legacy observations must never gain a clean dispute status during upgrade.
	if _, err := acquirerDB.Exec(ctx, "ALTER TABLE sim_charge DROP COLUMN dispute_status"); err != nil {
		t.Fatal(err)
	}
	upgraded, err := simulator.AcquirerWithKeys(ctx, acquirerDB, addr, addr, "sim-processor", "sim-instruct")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/sim/charges/"+id+"/reversal-evidence", nil)
	req.Header.Set("Authorization", "Bearer sim-processor")
	rec := httptest.NewRecorder()
	upgraded.ServeHTTP(rec, req)
	var source acquirerReversals
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &source) != nil || source.DisputeStatus != "unknown" {
		t.Fatal("upgraded payment missing its history was treated as clean", rec.Code, rec.Body.String())
	}
	// Seed an oversized, fully covered history on a separate source capture.
	history := call("POST", "/v1/payments", "many-refunds", PaymentInput{"MIMI-LARGE", "terminal", 1000, "USD", "visa", "MIMI-01", false}, 201)
	_, err = acquirerDB.Exec(ctx, `INSERT INTO sim_refund (operation_id,charge_reference,amount,merchant_id,currency)
	 SELECT 'history-'||n,$1,1,'mimi','USD' FROM generate_series(1,$2::int) n`, history["reference"], processor.MaxEvidenceRefunds+1)
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("GET", "/sim/charges/"+history["id"].(string)+"/reversal-evidence", nil)
	req.Header.Set("Authorization", "Bearer sim-processor")
	rec = httptest.NewRecorder()
	upgraded.ServeHTTP(rec, req)
	if rec.Code != 409 {
		t.Fatalf("oversized history supplied as a proof: %d", rec.Code)
	}
}

func TestReversalEvidenceRejectsIncompleteOrMisboundSource(t *testing.T) {
	p := Payment{ID: "dp_original", MerchantID: "mimi", PaymentInput: PaymentInput{"MIMI-order", "terminal", 720, "USD", "visa", "MIMI-01", false},
		Status: "captured", Reference: "ch_original", Refunded: 720}
	r := Refund{ID: "dr_original", MerchantID: "mimi", PaymentID: p.ID, Amount: 720, Currency: "USD", Reference: "rf_original", Status: "succeeded"}
	charge := chargeFact{OperationID: p.ID, Reference: p.Reference, MerchantID: "mimi", Status: "captured", OrderID: p.OrderID,
		Channel: p.Channel, Amount: p.Amount, Currency: p.Currency, Brand: p.Brand, TerminalID: p.TerminalID}
	refund := refundFact{refundRequest: originalRefund(r, p), Reference: r.Reference, Status: "succeeded"}
	for name, modify := range map[string]func(*acquirerReversals){
		"omitted completeness": func(a *acquirerReversals) { a.Complete = false },
		"omitted refunds":      func(a *acquirerReversals) { a.Refunds = nil },
		"missing refund":       func(a *acquirerReversals) { a.Refunds = []refundFact{} },
		"omitted dispute":      func(a *acquirerReversals) { a.DisputeStatus = "" },
		"unknown disposition":  func(a *acquirerReversals) { a.DisputeStatus = "pending-review" },
		"foreign merchant":     func(a *acquirerReversals) { a.Charge.MerchantID = "another" },
		"wrong order":          func(a *acquirerReversals) { a.Charge.OrderID = "another" },
		"unconfirmed capture":  func(a *acquirerReversals) { a.Charge.Status = "authorized" },
		"wrong capture ref":    func(a *acquirerReversals) { a.Charge.Reference = "ch_other" },
		"wrong refund parent":  func(a *acquirerReversals) { a.Refunds[0].ChargeReference = "ch_other" },
		"wrong amount":         func(a *acquirerReversals) { a.Refunds[0].Amount-- },
		"wrong currency":       func(a *acquirerReversals) { a.Refunds[0].Currency = "EUR" },
		"substituted refund":   func(a *acquirerReversals) { a.Refunds[0].OperationID = "dr_other" },
		"wrong refund ref":     func(a *acquirerReversals) { a.Refunds[0].Reference = "rf_other" },
		"pending refund":       func(a *acquirerReversals) { a.Refunds[0].Status = "pending" },
		"extra remote refund":  func(a *acquirerReversals) { a.Refunds = append(a.Refunds, refund) },
	} {
		t.Run(name, func(t *testing.T) {
			src := acquirerReversals{Charge: charge, Refunds: []refundFact{refund}, Complete: true, DisputeStatus: "none"}
			modify(&src)
			if result, err := matchReversals(p, []Refund{r}, src); err == nil || result.Complete {
				t.Fatal("inconsistent source accepted", result)
			}
		})
	}
}

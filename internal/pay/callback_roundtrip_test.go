package pay

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"dakimpay/internal/simulator"
)

// The only injected failures are the two lost HTTP responses. Advance the
// sender's retry deadline explicitly so the test never relies on wall-clock sleeps.
func TestCaptureReplyAndCallbackAcknowledgementBothLost(t *testing.T) {
	ctx := t.Context()
	const signingKey = "separate-synthetic-processor-signing-key"
	payDB, acquirerDB, bankDB := isolatedPool(t), isolatedPool(t), isolatedPool(t)
	bankHandler, err := simulator.BankWithKeys(ctx, bankDB, "read", "instruct", "post")
	if err != nil {
		t.Fatal(err)
	}
	bank := httptest.NewServer(bankHandler)
	defer bank.Close()
	acq := httptest.NewUnstartedServer(nil)
	acquirerHandler, err := simulator.AcquirerWithKeys(ctx, acquirerDB, "http://"+acq.Listener.Addr().String(), bank.URL, "read", "instruct", "approve")
	if err != nil {
		t.Fatal(err)
	}
	var chargeSubmissions atomic.Int32
	acq.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/sim/charges" {
			chargeSubmissions.Add(1)
		}
		acquirerHandler.ServeHTTP(w, r)
	})
	acq.Start()
	defer acq.Close()
	openService := func() *Service {
		t.Helper()
		s, err := New(ctx, payDB, acq.URL, bank.URL, "read")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	service := openService()
	code, p := issue(service.Handler(), "POST", "/v1/payments", "both-replies-lost", PaymentInput{"BOTH-REPLIES-LOST", "terminal", 1234, "USD", "visa", "MIMI-01", true})
	if code != 202 || p["status"] != "unknown" {
		t.Fatalf("missing terminal evidence: %d %v", code, p)
	}
	id := p["id"].(string)

	receive := service.CallbackHandler(signingKey)
	broken := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		result := httptest.NewRecorder()
		receive.ServeHTTP(result, r)
		if result.Code/100 != 2 {
			t.Errorf("first delivery: %d %s", result.Code, result.Body.String())
		}
		panic(http.ErrAbortHandler) // receiver already committed its inbox row
	}))
	claimed, err := simulator.DeliverCallback(ctx, acquirerDB, broken.URL+"/callbacks/processor", signingKey)
	broken.Close()
	if !claimed || err == nil {
		t.Fatal("expected lost acknowledgement")
	}
	service = openService()
	live := httptest.NewServer(service.CallbackHandler(signingKey))
	defer live.Close()
	tag, err := acquirerDB.Exec(ctx, "UPDATE sim_callback_outbox SET next_attempt_at=now() WHERE acknowledged_at IS NULL AND operation_id=$1", id)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("unacknowledged delivery: %v %v", tag, err)
	}
	deliver := func() {
		t.Helper()
		worked, err := simulator.DeliverCallback(ctx, acquirerDB, live.URL+"/callbacks/processor", signingKey)
		if !worked || err != nil {
			t.Fatalf("deliver: %v, %v", worked, err)
		}
		if err = service.ProcessCallbacks(ctx); err != nil {
			t.Fatal(err)
		}
	}
	deliver()
	call := func(method, path, key string, body any, want int) obj {
		t.Helper()
		code, data := issue(service.Handler(), method, path, key, body)
		if code != want {
			t.Fatalf("%s -> %d %v", path, code, data)
		}
		return data
	}
	if got := call("GET", "/v1/payments/"+id, "", nil, 200); got["status"] != "captured" || chargeSubmissions.Load() != 1 {
		t.Fatalf("capture recovery: %v, %d submissions", got, chargeSubmissions.Load())
	}
	// Two original-card refunds exactly consume the capture; the third must fail.
	for _, part := range []struct {
		key    string
		amount int
	}{{"part", 434}, {"remainder", 800}} {
		call("POST", "/v1/payments/"+id+"/refunds", part.key, obj{"amount": part.amount}, 201)
		deliver()
	}
	call("POST", "/v1/payments/"+id+"/refunds", "extra-cent", obj{"amount": 1}, 409)
	callbacks := call("GET", "/v1/dashboard", "", nil, 200)["callbacks"].([]any)
	if len(callbacks) != 3 {
		t.Fatalf("duplicate or missing callback: %v", callbacks)
	}
	for _, v := range callbacks {
		c := v.(map[string]any)
		if c["status"] != "applied" || c["attempts"] != float64(1) || c["conflicts"] != float64(0) {
			t.Fatal(c)
		}
	}
	payout := call("POST", "/v1/settle", "fees-only", nil, 201)
	if payout["net"] != float64(-25) || len(payout["lines"].([]any)) != 4 {
		t.Fatalf("statement with full return: %v", payout)
	}
	statementURL := "/v1/payouts/" + payout["id"].(string) + "/statement"
	call("GET", statementURL, "", nil, 409)
	req, err := http.NewRequestWithContext(ctx, "POST", bank.URL+"/bank/post/"+payout["bank_trace"].(string), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer post")
	res, err := bank.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode/100 != 2 {
		t.Fatal("bank posting", res.StatusCode)
	}
	if matched := call("POST", "/v1/reconcile", "", nil, 200); matched["matched"] != float64(1) {
		t.Fatal(matched)
	}
	statement := call("GET", statementURL, "", nil, 200)
	rows := statement["lines"].([]any)
	if statement["depositMinor"] != float64(-25) || len(rows) != 3 {
		t.Fatal(statement)
	}
	net := 0
	for _, raw := range rows {
		row := raw.(map[string]any)
		net += int(row["grossMinor"].(float64)) - int(row["feeMinor"].(float64))
	}
	if net != -25 {
		t.Fatal("merchant statement does not explain bank debit", rows)
	}
}

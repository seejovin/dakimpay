package pay

import (
	"context"
	"dakimpay/internal/simulator"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A refund after the original batch's cutoff is a separate bank debit. Exercise
// the real simulation services with separate stores, earnings and signed offsets.
func TestRefundAfterRemittanceCreatesIndependentDebit(t *testing.T) {
	ctx := context.Background()
	bankHandler, err := simulator.BankWithKeys(ctx, isolatedPool(t), "service", "instruct", "post")
	if err != nil {
		t.Fatal(err)
	}
	bank := httptest.NewServer(bankHandler)
	defer bank.Close()
	aq := httptest.NewUnstartedServer(nil)
	acquirer, err := simulator.AcquirerWithKeys(ctx, isolatedPool(t), "http://"+aq.Listener.Addr().String(), bank.URL, "service", "instruct", "approve")
	if err != nil {
		t.Fatal(err)
	}
	aq.Config.Handler = acquirer
	aq.Start()
	defer aq.Close()
	service, err := New(ctx, isolatedPool(t), aq.URL, bank.URL, "service")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, key string, in any, want int) obj {
		t.Helper()
		code, result := issue(service.Handler(), method, path, key, in)
		if code != want {
			t.Fatalf("%s %s = %d: %v", method, path, code, result)
		}
		return result
	}
	postAndMatch := func(p obj) {
		t.Helper()
		r, err := http.NewRequest("POST", bank.URL+"/bank/post/"+p["bank_trace"].(string), nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer post")
		res, err := bank.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode/100 != 2 {
			t.Fatal("bank", res.StatusCode)
		}
		call("POST", "/v1/reconcile", "", nil, 200)
	}
	pay := call("POST", "/v1/payments", "A", PaymentInput{"LATE-RETURN", "terminal", 1000, "USD", "amex", "MIMI-01", false}, 201)
	p := call("POST", "/v1/settle", "FIRST", nil, 201)
	if p["net"] != float64(980) {
		t.Fatal(p)
	}
	postAndMatch(p)
	ref := call("POST", "/v1/payments/"+pay["id"].(string)+"/refunds", "REF", obj{"amount": 400}, 201)
	debit := call("POST", "/v1/settle", "SECOND", nil, 201)
	if debit["net"] != float64(-400) || debit["bank_trace"] == p["bank_trace"] {
		t.Fatal(debit)
	}
	call("GET", "/v1/payouts/"+debit["id"].(string)+"/statement", "", nil, 409)
	postAndMatch(debit)
	statement := call("GET", "/v1/payouts/"+debit["id"].(string)+"/statement", "", nil, 200)
	rows := statement["lines"].([]any)
	if statement["depositMinor"] != float64(-400) || len(rows) != 1 {
		t.Fatal(statement)
	}
	line := rows[0].(map[string]any)
	if line["reference"] != ref["id"] || line["grossMinor"] != float64(-400) || line["feeMinor"] != float64(0) {
		t.Fatal(line)
	}
	dashboard := call("GET", "/v1/dashboard", "", nil, 200)
	var operation string
	for _, item := range dashboard["fee_reports"].([]any) {
		link := item.(map[string]any)
		if link["payout_id"] == debit["id"] {
			operation = link["operation_id"].(string)
		}
	}
	report := call("GET", "/v1/settlements/"+operation+"/fee-report", "", nil, 200)
	if report["status"] != "no_fees" || report["bank_status"] != "matched" || report["bank_amount"] != float64(-400) ||
		report["actual_total"] != float64(0) || report["expected_total"] != float64(0) || len(report["lines"].([]any)) != 0 {
		t.Fatal("fee report for a refund-only bank debit", report)
	}
	totals := dashboard["totals"].(map[string]any)
	if totals["deposited"] != float64(580) || totals["fees"] != float64(20) {
		t.Fatal(totals)
	}
}

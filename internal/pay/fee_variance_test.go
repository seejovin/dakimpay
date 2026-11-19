package pay

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dakimpay/internal/simulator"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Fingerprint all rows, including values such as status, references and posting
// times. A cash balance alone would miss offsetting entries or rewritten fees.
func feeLedgerFingerprint(t *testing.T, db *pgxpool.Pool, schema string) map[string]string {
	t.Helper()
	ctx := t.Context()
	rows, err := db.Query(ctx, "SELECT tablename FROM pg_tables WHERE schemaname=$1 ORDER BY tablename", schema)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(tables) == 0 {
		t.Fatalf("list fixture tables in %s: %v (%v)", schema, tables, err)
	}
	evidence := make(map[string]string, len(tables))
	for _, table := range tables {
		var fingerprint string
		name := pgx.Identifier{schema, table}.Sanitize()
		err := db.QueryRow(ctx, "SELECT md5(coalesce(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t))::text,'[]')) FROM "+name+" t").Scan(&fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		evidence[table] = fingerprint
	}
	return evidence
}

func TestItemizedFeeVarianceKeepsMatchedCashAndOriginals(t *testing.T) {
	ctx := t.Context()
	db, processorDB, bankDB := isolatedPool(t), isolatedPool(t), isolatedPool(t)
	bankHandler, err := simulator.BankWithKeys(ctx, bankDB, "read", "instruct", "post")
	if err != nil {
		t.Fatal(err)
	}
	var reading atomic.Bool
	guard := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if reading.Load() && r.Method != http.MethodGet {
				t.Errorf("read-only fee report attempted %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	var bankReadMode atomic.Int32
	bank := httptest.NewServer(guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/bank/entries/") {
			switch bankReadMode.Load() {
			case 1:
				trace := strings.TrimPrefix(r.URL.Path, "/bank/entries/")
				answer(w, 200, bankEntry{Trace: trace, MerchantID: "mimi", Currency: "USD", Amount: 2939, Status: "posted"})
				return
			case 2:
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		bankHandler.ServeHTTP(w, r)
	})))
	defer bank.Close()
	processor := httptest.NewUnstartedServer(nil)
	handler, err := simulator.AcquirerWithKeys(ctx, processorDB, "http://"+processor.Listener.Addr().String(), bank.URL, "read", "instruct", "approve")
	if err != nil {
		t.Fatal(err)
	}
	var sourceReadMode atomic.Int32
	var controlFeeLine string
	processor.Config.Handler = guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sourceReadMode.Load() == 0 || !strings.HasSuffix(r.URL.Path, "/fee-evidence") {
			handler.ServeHTTP(w, r)
			return
		}
		// A previous processor version supplied valid frozen statement lines
		// without a capture-time quote. Keep the money records intact.
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		var evidence map[string]json.RawMessage
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &evidence) != nil {
			t.Errorf("expected valid simulator fee evidence: HTTP %d", response.Code)
			http.Error(w, "bad fixture", 500)
			return
		}
		switch sourceReadMode.Load() {
		case 1:
			evidence["estimates"] = json.RawMessage("[]")
		case 2:
			// Bind every estimate to its actual merchant payment.
			var estimates []map[string]json.RawMessage
			if err := json.Unmarshal(evidence["estimates"], &estimates); err != nil || len(estimates) == 0 {
				t.Errorf("read estimates: %v", err)
				w.WriteHeader(500)
				return
			}
			estimates[0]["payment_id"] = json.RawMessage(`"other-payment"`)
			evidence["estimates"], _ = json.Marshal(estimates)
		case 3:
			// Deliberately ambiguous wire evidence. Both occurrences have the
			// same value, so a last-key-wins decoder would wrongly accept it.
			raw := bytes.TrimSpace(response.Body.Bytes())
			w.Header().Set("Content-Type", "application/json")
			w.Write(append([]byte(`{"currency":"USD",`), raw[1:]...))
			return
		case 4:
			delete(evidence, "estimates")
		case 5:
			var lines []map[string]json.RawMessage
			if err := json.Unmarshal(evidence["lines"], &lines); err != nil {
				t.Errorf("read statement lines: %v", err)
				w.WriteHeader(500)
				return
			}
			for _, line := range lines {
				if string(line["kind"]) == `"fee"` {
					line["id"], _ = json.Marshal(controlFeeLine)
					break
				}
			}
			evidence["lines"], _ = json.Marshal(lines)
		}
		answer(w, 200, evidence)
	}))
	processor.Start()
	defer processor.Close()

	start := func() *Service {
		t.Helper()
		s, err := New(ctx, db, processor.URL, bank.URL, "read")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := start()
	call := func(method, path, key string, payload any, expected int) obj {
		t.Helper()
		code, body := issue(s.Handler(), method, path, key, payload)
		if code != expected {
			t.Fatalf("%s %s: got %d want %d: %v", method, path, code, expected, body)
		}
		return body
	}
	sale := func(order string, amount int64) obj {
		t.Helper()
		result := call("POST", "/v1/payments", order, PaymentInput{OrderID: order, Channel: "terminal", Amount: amount, Currency: "USD", Brand: "visa", TerminalID: "MIMI-01"}, http.StatusCreated)
		if result["status"] != "captured" || result["reference"] == "" {
			t.Fatal(result)
		}
		return result
	}
	postBank := func(trace string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, bank.URL+"/bank/post/"+trace, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer post")
		res, err := bank.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal(res.StatusCode)
		}
	}
	operation := func(key string) string {
		t.Helper()
		var op string
		if err := db.QueryRow(ctx, "SELECT operation_id FROM pay.settlement_requests WHERE request_key=$1", key).Scan(&op); err != nil {
			t.Fatal(err)
		}
		return op
	}
	report := func(op string) obj {
		t.Helper()
		reading.Store(true)
		defer reading.Store(false)
		return call("GET", "/v1/settlements/"+op+"/fee-report", "", nil, 200)
	}
	// First match a different payout through the ordinary bank workflow. Its
	// receipt, statement and posted bank entry must survive the later report.
	control := sale("FEE-CONTROL", 100)
	matched := call("POST", "/v1/settle", "CONTROL", nil, 201)
	postBank(matched["bank_trace"].(string))
	if res := call("POST", "/v1/reconcile", "", nil, 200); res["matched"] != float64(1) {
		t.Fatal(res)
	}
	controlPath := "/v1/payouts/" + matched["id"].(string) + "/statement"
	paidStatement := call("GET", controlPath, "", nil, 200)
	if paidStatement["depositMinor"] != float64(98) {
		t.Fatal(paidStatement)
	}
	controlOp := operation("CONTROL")
	aligned := report(controlOp)
	controlFeeLine = aligned["lines"].([]any)[0].(map[string]any)["line_id"].(string)
	if aligned["status"] != "aligned" || aligned["different_count"] != float64(0) ||
		aligned["expected_total"] != float64(2) || aligned["actual_total"] != float64(2) ||
		aligned["bank_status"] != "matched" || aligned["local_status"] != "matched" {
		t.Fatal("matched control", control["id"], aligned)
	}

	first, second := sale("FEE-A", 1000), sale("FEE-B", 2000)
	// The processor recorded each fee estimate at capture. Supply independently
	// different itemized fees before the simulator freezes its remittance.
	updateSourceFee := func(ref string, fee int64) {
		t.Helper()
		tag, err := processorDB.Exec(ctx, "UPDATE sim_line SET amount=$2 WHERE reference=$1 AND kind='fee' AND payout_id IS NULL", ref, -fee)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("set pending source fee: %v, %v", tag, err)
		}
	}
	updateSourceFee(first["reference"].(string), 25)
	updateSourceFee(second["reference"].(string), 35)
	call("POST", "/v1/settle", "VARIANCE", nil, 409)
	op := operation("VARIANCE")
	var payout, trace string
	var net int64
	if err := processorDB.QueryRow(ctx, "SELECT id,bank_trace,net FROM sim_payout WHERE operation_id=$1", op).Scan(&payout, &trace, &net); err != nil || net != 2940 {
		t.Fatal("source payout", net, err)
	}

	beforeBank := report(op)
	if beforeBank["bank_status"] != "pending" || beforeBank["local_status"] != "unbooked" ||
		beforeBank["different_count"] != float64(2) || beforeBank["variance"] != float64(0) {
		t.Fatal("before independent bank posting", beforeBank)
	}
	postBank(trace)
	var posted int64
	if err := bankDB.QueryRow(ctx, "SELECT amount FROM sim_bank_entry WHERE trace=$1 AND currency='USD' AND merchant_id='mimi' AND posted_at IS NOT NULL", trace).Scan(&posted); err != nil || posted != 2940 {
		t.Fatal("independent posting", posted, err)
	}

	snapshot := func() []map[string]string {
		t.Helper()
		return []map[string]string{
			feeLedgerFingerprint(t, db, "pay"),
			feeLedgerFingerprint(t, processorDB, "public"),
			feeLedgerFingerprint(t, bankDB, "public"),
		}
	}
	before := snapshot()
	result := report(op)
	for _, scenario := range []struct {
		mode   int32
		status string
	}{{1, "mismatch"}, {2, "unavailable"}} {
		bankReadMode.Store(scenario.mode)
		held := report(op)
		if held["bank_status"] != scenario.status || held["local_status"] != "unbooked" ||
			held["different_count"] != float64(2) {
			t.Fatalf("bank %s: %+v", scenario.status, held)
		}
	}
	bankReadMode.Store(0)
	for k, want := range map[string]any{"operation_id": op, "payout_id": payout, "merchant_id": "mimi", "currency": "USD", "bank_trace": trace,
		"bank_status": "matched", "local_status": "unbooked", "status": "variance",
		"net": float64(2940), "bank_amount": float64(2940), "expected_total": float64(60), "actual_total": float64(60), "variance": float64(0), "different_count": float64(2), "missing_count": float64(0)} {
		if got := result[k]; got != want {
			t.Fatalf("%s: got %v, want %v (%+v)", k, got, want, result)
		}
	}
	wantByPayment := map[string]struct {
		sale       obj
		fee, delta float64
	}{
		first["id"].(string): {first, 25, 5}, second["id"].(string): {second, 35, -5},
	}
	lines, ok := result["lines"].([]any)
	if !ok || len(lines) != 2 {
		t.Fatal("itemized fee lines", result["lines"])
	}
	for _, raw := range lines {
		item := raw.(map[string]any)
		id := item["payment_id"].(string)
		want, ok := wantByPayment[id]
		if !ok {
			t.Fatal("duplicate or unexpected payment", id)
		}
		delete(wantByPayment, id)
		ref := want.sale["reference"].(string)
		var quoteTime time.Time
		var chargeFee int64
		var line string
		err := processorDB.QueryRow(ctx, `SELECT e.recorded_at,e.fee,l.id
			FROM sim_fee_estimate e JOIN sim_line l ON l.reference=e.reference
			WHERE e.payment_id=$1 AND e.reference=$2 AND l.payout_id=$3 AND l.kind='fee'`, id, ref, payout).Scan(&quoteTime, &chargeFee, &line)
		if err != nil {
			t.Fatal(err)
		}
		recorded, err := time.Parse(time.RFC3339Nano, item["recorded_at"].(string))
		if err != nil || !quoteTime.Equal(recorded) {
			t.Fatal("source estimate timestamp", quoteTime, recorded, err)
		}
		if item["order_id"] != want.sale["order_id"] || item["reference"] != ref || item["line_id"] != line ||
			item["gross"] != want.sale["amount"] || item["expected"] != want.fee-want.delta || item["expected"] != float64(chargeFee) ||
			item["actual"] != want.fee || item["variance"] != want.delta || item["rate_bps"] != float64(200) || item["policy"] != "sim-card-fees-v1" {
			t.Fatal("fee or provenance mismatch", item, want.sale)
		}
	}
	dashboard := call("GET", "/v1/dashboard", "", nil, 200)
	summaries, ok := dashboard["fee_reports"].([]any)
	if !ok {
		t.Fatal("missing finance report list", dashboard)
	}
	flags := map[string]string{}
	for _, item := range summaries {
		entry := item.(map[string]any)
		flags[entry["operation_id"].(string)] = entry["local_status"].(string)
	}
	if flags[controlOp] != "matched" || flags[op] != "unbooked" {
		t.Fatal("report not discoverable by original settlement operation", flags)
	}

	sourceReadMode.Store(1)
	missing := report(controlOp)
	if missing["status"] != "incomplete" || missing["missing_count"] != float64(1) || missing["expected_total"] != nil ||
		missing["variance"] != nil || missing["actual_total"] != float64(2) || missing["bank_status"] != "matched" || missing["local_status"] != "matched" {
		t.Fatal("unavailable capture-time estimate misreported", missing)
	}
	missingLines := missing["lines"].([]any)
	if len(missingLines) != 1 {
		t.Fatal("lost original statement item", missingLines)
	}
	missingLine := missingLines[0].(map[string]any)
	if missingLine["reference"] != control["reference"] || missingLine["actual"] != float64(2) ||
		missingLine["expected"] != nil || missingLine["variance"] != nil || missingLine["recorded_at"] != nil {
		t.Fatal("unavailable historical quote fabricated or actual fee lost", missingLine)
	}
	sourceReadMode.Store(0)
	if restored := report(controlOp); !reflect.DeepEqual(restored, aligned) {
		t.Fatal("original control changed", aligned, restored)
	}

	reading.Store(true)
	for _, scenario := range []struct {
		mode int32
		code int
	}{{2, 409}, {3, 502}, {4, 409}, {5, 409}} {
		sourceReadMode.Store(scenario.mode)
		call("GET", "/v1/settlements/"+op+"/fee-report", "", nil, scenario.code)
	}
	sourceReadMode.Store(0)
	reading.Store(false)
	call("GET", "/v1/payouts/"+payout+"/statement", "", nil, 404)
	for i := 0; i < 2; i++ {
		s = start()
		if got := report(op); !reflect.DeepEqual(got, result) {
			t.Fatal("report changed after restart/read", got, result)
		}
		if got := call("GET", controlPath, "", nil, 200); !reflect.DeepEqual(got, paidStatement) {
			t.Fatal("previous bank statement changed", got, paidStatement)
		}
	}
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatal("report changed durable pay, simulator or bank rows", before, after)
	}
	t.Logf("2 itemized variances (20 -> 25 and 40 -> 35 cents), $29.40 cash matched, previous posted payout unchanged, %d financial and operational tables unchanged",
		len(before[0])+len(before[1])+len(before[2]))
}

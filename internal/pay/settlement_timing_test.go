package pay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync/atomic"
	"testing"

	"dakimpay/internal/simulator"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A processor statement and the bank's posted-entry inventory are independent
// event streams. Advance their source times explicitly; no worker needs to
// poll, sleep, dispatch a transfer or reconcile Mimi's local ledger.
func TestSettlementTimingAtFixedCutoffsPreservesSourceRecords(t *testing.T) {
	ctx := t.Context()
	merchant, acquirer, bank := isolatedPool(t), isolatedPool(t), isolatedPool(t)
	const (
		sourceDown int32 = iota + 1
		incomplete
		wrongCutoff
		duplicateTrace
		wrongMerchant
		wrongCurrency
		futureRecord
		omittedAmount
		reusedPayoutID
	)
	var bankFault, acquirerFault atomic.Int32
	source := func(handler http.Handler, path string, fault *atomic.Int32) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != path || r.URL.Query().Get("as_of") == "" {
				t.Errorf("unexpected external operation during timing report: %s %s", r.Method, r.URL)
				http.Error(w, "unexpected operation", 405)
				return
			}
			problem := fault.Load()
			switch problem {
			case sourceDown:
				http.Error(w, "source unavailable", 503)
			case incomplete, wrongCutoff, duplicateTrace, wrongMerchant, wrongCurrency, futureRecord, omittedAmount, reusedPayoutID:
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, r)
				var packet obj
				if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &packet) != nil {
					t.Errorf("invalid source fixture: %d", rec.Code)
					http.Error(w, "fixture error", 500)
					return
				}
				records := packet["records"].([]any)
				first := records[0].(map[string]any)
				switch problem {
				case incomplete:
					packet["complete"] = false
				case wrongCutoff:
					packet["as_of"] = "2026-10-07T11:59:59Z"
				case duplicateTrace:
					packet["records"] = append(records, first)
				case wrongMerchant:
					first["merchant_id"] = "other-store"
				case wrongCurrency:
					first["currency"] = "EUR"
				case futureRecord:
					field := "posted_at"
					if _, ok := first["created_at"]; ok {
						field = "created_at"
					}
					first[field] = "2026-10-07T12:00:00.000001Z"
				case omittedAmount:
					for _, value := range records {
						row := value.(map[string]any)
						if row["amount"] == 0.0 {
							delete(row, "amount")
							break
						}
					}
				case reusedPayoutID:
					second := records[1].(map[string]any)
					second["payout_id"] = first["payout_id"]
				}
				answer(w, 200, packet)
			default:
				handler.ServeHTTP(w, r)
			}
		})
	}
	bankHandler, err := simulator.BankWithKeys(ctx, bank, "read", "instruct", "post")
	if err != nil {
		t.Fatal(err)
	}
	bankHTTP := httptest.NewServer(source(bankHandler, "/bank/settlement-timing", &bankFault))
	defer bankHTTP.Close()
	acquirerHTTP := httptest.NewUnstartedServer(nil)
	acquirerHandler, err := simulator.AcquirerWithKeys(ctx, acquirer, "http://"+acquirerHTTP.Listener.Addr().String(), bankHTTP.URL, "read", "instruct", "approve")
	if err != nil {
		t.Fatal(err)
	}
	acquirerHTTP.Config.Handler = source(acquirerHandler, "/sim/settlement-timing", &acquirerFault)
	acquirerHTTP.Start()
	defer acquirerHTTP.Close()
	newService := func() http.Handler {
		t.Helper()
		s, err := New(ctx, merchant, acquirerHTTP.URL, bankHTTP.URL, "read")
		if err != nil {
			t.Fatal(err)
		}
		return s.Handler()
	}
	h := newService()
	exec := func(db *pgxpool.Pool, sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Each suffix is the trace across both sources. Equal net amounts, an
	// instruction without a posting, and a local match are insufficient alone.
	payout := func(suffix string, amount int64, at string) {
		t.Helper()
		exec(acquirer, `INSERT INTO sim_payout(id,operation_id,bank_trace,merchant_id,currency,batch,net,created_at)
			VALUES($1,$2,$3,'mimi','USD','standard',$4,$5::timestamptz)`,
			"po-"+suffix, "ds-"+suffix, "bt-"+suffix, amount, at)
	}
	post := func(suffix string, amount int64, at string) {
		t.Helper()
		exec(bank, `INSERT INTO sim_bank_entry(trace,merchant_id,currency,amount,received_at,posted_at)
			VALUES($1,'mimi','USD',$2,$3::timestamptz - interval '1 minute',$3::timestamptz)`, "bt-"+suffix, amount, at)
	}
	const day = "2026-10-07T"
	const noon = day + "12:00:00Z"
	payout("control", 98, day+"08:00:00Z")
	post("control", 98, day+"08:05:00Z")
	exec(merchant, `INSERT INTO pay.payments(id,merchant_id,request_key,order_id,channel,amount,currency,brand,terminal_id,lose_response,status,reference,created_at)
		 VALUES('dp-control','mimi','sale','order','terminal',100,'USD','visa','MIMI-01',false,'captured','ch-control','2026-10-07T07:55:00Z');
		 INSERT INTO pay.settlement_requests VALUES('settle-control','ds-control','standard',false);
		 INSERT INTO pay.payouts(id,operation_id,bank_trace,currency,net,status,bank_evidence,created_at)
		 VALUES('po-control','ds-control','bt-control','USD',98,'matched','{"trace":"bt-control","merchant_id":"mimi","currency":"USD","amount":98,"status":"posted"}','2026-10-07T08:01:00Z');
		 INSERT INTO pay.lines VALUES('capture-control','po-control','capture','ch-control',100),('fee-control','po-control','fee','ch-control',-2);
		 INSERT INTO pay.events(subject,kind,message,created_at) VALUES('po-control','matched','Matched control posting','2026-10-07T08:06:00Z');`)
	payout("transit", 2940, day+"09:30:00Z")
	post("bank-only", -300, day+"10:10:00Z")
	payout("different", -300, day+"09:00:00Z")
	post("different", -299, day+"09:05:00Z")
	exec(bank, `INSERT INTO sim_bank_entry(trace,merchant_id,currency,amount,received_at)
		  VALUES('bt-instruction','mimi','USD',2940,'2026-10-07T07:59:00Z')`)

	payout("zero", 0, day+"14:00:00Z")
	post("zero", 0, day+"14:00:00Z")
	// Even a locally stored amount and trace can be accompanied by an
	// impossible booking time. Preserve the genuine two-source match.
	exec(merchant, `INSERT INTO pay.settlement_requests VALUES('settle-zero','ds-zero','standard',false);
        INSERT INTO pay.payouts(id,operation_id,bank_trace,currency,net,status,bank_evidence,created_at)
        VALUES('po-zero','ds-zero','bt-zero','USD',0,'matched','{"trace":"bt-zero","merchant_id":"mimi","currency":"USD","amount":0,"status":"posted"}','2026-10-07T13:57:00Z');
        INSERT INTO pay.events(subject,kind,message,created_at) VALUES('po-zero','matched','Pre-dated observation','2026-10-07T13:59:59Z');`)

	snapshot := func() []map[string]string {
		t.Helper()
		return []map[string]string{feeLedgerFingerprint(t, merchant, "pay"), feeLedgerFingerprint(t, acquirer, "public"), feeLedgerFingerprint(t, bank, "public")}
	}
	// Later observations are appended to their independent stores. Compare
	// all pre-existing rows as well as whole database snapshots around reads.
	retained := func() [2]string {
		t.Helper()
		var a, b string
		if err := acquirer.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id)::text FROM sim_payout p WHERE id<>'po-bank-only'`).Scan(&a); err != nil {
			t.Fatal(err)
		}
		if err := bank.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(b) ORDER BY b.trace)::text FROM sim_bank_entry b WHERE trace<>'bt-transit'`).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return [2]string{a, b}
	}
	const path = "/v1/settlement-timing"
	report := func(at string) obj {
		t.Helper()
		code, result := issue(h, http.MethodGet, path+"?as_of="+url.QueryEscape(at), "", nil)
		if code != 200 || result["merchant_id"] != "mimi" || result["currency"] != "USD" {
			t.Fatalf("report at %s: %d %v", at, code, result)
		}
		return result
	}
	find := func(result obj, section, ref string) obj {
		t.Helper()
		rows, ok := result[section].([]any)
		if !ok {
			t.Fatalf("missing %s rows: %v", section, result)
		}
		for _, raw := range rows {
			row := raw.(map[string]any)
			if row["bank_trace"] == "bt-"+ref {
				return row
			}
		}
		t.Fatalf("missing %s %s: %v", section, ref, result)
		return nil
	}
	want := func(row obj, fields obj) {
		t.Helper()
		for key, v := range fields {
			value, present := row[key]
			if !present || value != v {
				t.Errorf("%s: %s=%v present=%v; want %v (%v)", row["bank_trace"], key, value, present, v, row)
			}
		}
	}
	count := func(result obj, open, matched int) {
		t.Helper()
		if len(result["open"].([]any)) != open || len(result["matched"].([]any)) != matched {
			t.Fatalf("want %d open and %d matched at %s: %v", open, matched, result["as_of"], result)
		}
	}
	initial, originalRows := snapshot(), retained()
	midday := report(noon)
	count(midday, 3, 1)
	want(midday, obj{"as_of": noon})
	want(find(midday, "open", "transit"), obj{"status": "in_transit", "age_seconds": 9000.0, "processor_amount": 2940.0, "bank_amount": nil, "currency": "USD", "operation_id": "ds-transit", "payout_id": "po-transit", "payout_at": day + "09:30:00Z", "posted_at": nil, "booked_match_at": nil})
	want(find(midday, "open", "bank-only"), obj{"status": "bank_only", "age_seconds": 6600.0, "bank_amount": -300.0, "processor_amount": nil, "currency": "USD", "operation_id": "", "payout_id": "", "payout_at": nil, "posted_at": day + "10:10:00Z", "booked_match_at": nil})
	want(find(midday, "open", "different"), obj{"status": "amount_mismatch", "age_seconds": 10800.0, "processor_amount": -300.0, "bank_amount": -299.0})
	want(find(midday, "matched", "control"), obj{"status": "matched", "age_seconds": 300.0, "booked_match_at": day + "08:06:00Z"})
	// An older cutoff cannot inherit even a genuine present-day booked match.
	want(find(report(day+"08:05:00Z"), "matched", "control"), obj{"booked_match_at": nil, "age_seconds": 300.0})
	count(report(day+"07:59:59Z"), 0, 0)
	if !reflect.DeepEqual(report("2026-10-07T20:00:00.000000099+08:00"), midday) {
		t.Fatal("equivalent UTC cutoff produced a different queue")
	}
	for _, sourceFault := range []*atomic.Int32{&acquirerFault, &bankFault} {
		faults := []int32{sourceDown, incomplete, wrongCutoff, duplicateTrace, wrongMerchant, wrongCurrency, futureRecord, omittedAmount}
		if sourceFault == &acquirerFault {
			faults = append(faults, reusedPayoutID)
		}
		for _, fault := range faults {
			sourceFault.Store(fault)
			cutoff := noon
			if fault == omittedAmount {
				cutoff = day + "14:00:00Z"
			}
			code, result := issue(h, http.MethodGet, path+"?as_of="+cutoff, "", nil)
			if code/100 == 2 || result["open"] != nil || result["matched"] != nil {
				t.Fatalf("incomplete source produced a queue: fault=%d %d %v", fault, code, result)
			}
			sourceFault.Store(0)
		}
	}
	for _, q := range []string{"?as_of=bad", "?as_of=" + noon + "&as_of=" + noon, "?as_of=2999-01-01T00:00:00Z", "?other=1"} {
		if code, out := issue(h, http.MethodGet, path+q, "", nil); code != 422 {
			t.Fatalf("query %s accepted: %d %v", q, code, out)
		}
	}
	if !reflect.DeepEqual(initial, snapshot()) {
		t.Fatal("cutoff or incomplete-source report changed stored records")
	}

	post("transit", 2940, day+"12:15:00Z")
	payout("bank-only", -300, day+"12:30:00Z")
	afterAppend := snapshot()
	if retained() != originalRows || !reflect.DeepEqual(initial[0], afterAppend[0]) {
		t.Fatal("appending independent observations changed original source or merchant history")
	}
	if result := report(noon); !reflect.DeepEqual(result, midday) {
		t.Fatal("later arrivals changed the previous cutoff", result)
	}
	beforePosting := report(day + "12:14:59.999999Z")
	want(find(beforePosting, "open", "transit"), obj{"status": "in_transit", "age_seconds": 9899.0})
	atPosting := report(day + "12:15:00Z")
	count(atPosting, 2, 2)
	want(find(atPosting, "matched", "transit"), obj{"status": "matched", "age_seconds": 9900.0, "bank_amount": 2940.0, "processor_amount": 2940.0, "payout_at": day + "09:30:00Z", "posted_at": day + "12:15:00Z", "booked_match_at": nil})
	want(find(atPosting, "open", "bank-only"), obj{"status": "bank_only", "age_seconds": 7500.0})
	last := report(day + "12:30:00Z")
	count(last, 1, 3)
	want(find(last, "matched", "bank-only"), obj{"status": "matched", "age_seconds": 8400.0, "bank_amount": -300.0, "processor_amount": -300.0, "payout_at": day + "12:30:00Z", "posted_at": day + "10:10:00Z"})
	want(find(last, "open", "different"), obj{"status": "amount_mismatch"})
	equalTime := report(day + "14:00:00Z")
	want(find(equalTime, "matched", "zero"), obj{"processor_amount": 0.0, "bank_amount": 0.0, "age_seconds": 0.0, "booked_match_at": nil})
	h = newService()
	if got := report(day + "12:30:00Z"); !reflect.DeepEqual(got, last) {
		t.Fatal("restarted report changed", got)
	}
	if got := report(noon); !reflect.DeepEqual(got, midday) {
		t.Fatal("historical replay changed", got)
	}
	if !reflect.DeepEqual(afterAppend, snapshot()) {
		t.Fatal("report reads or restart changed stored records")
	}
	t.Logf("noon: three open, one matched; 12:15Z bank posting: two open; 12:30Z processor payout: one open, three matched; original source rows and all %d tables survived reads and restart",
		len(afterAppend[0])+len(afterAppend[1])+len(afterAppend[2]))
}

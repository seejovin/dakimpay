package pay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"dakimpay/internal/simulator"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Advance report delivery separately from the native effective timestamps.
// Both source databases are real disposable PostgreSQL databases; the HTTP
// wrappers only count calls and simulate an unavailable or contradictory feed.
func TestSettlementTimingKnowledgeSurvivesBackdatedDeliveryAndRestart(t *testing.T) {
	ctx := t.Context()
	merchant, processor, bank := isolatedPool(t), isolatedPool(t), isolatedPool(t)
	const day = "2026-10-07T"
	var unavailable, contradictory atomic.Bool
	var calls, now, deliveredAt atomic.Int64
	audit := func(h http.Handler, path string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.Method != http.MethodGet || r.URL.Path != path {
				t.Errorf("report attempted %s %s", r.Method, r.URL)
				w.WriteHeader(405)
				return
			}
			if unavailable.Load() {
				w.WriteHeader(503)
				return
			}
			if contradictory.Load() && path == "/sim/settlement-timing" {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				var packet obj
				if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &packet) != nil {
					t.Error("bad synthetic source response")
					w.WriteHeader(500)
					return
				}
				for _, item := range packet["records"].([]any) {
					row := item.(map[string]any)
					if row["bank_trace"] == "bt-grocery" {
						row["amount"] = 2941
					}
				}
				answer(w, 200, packet)
				return
			}
			if at := deliveredAt.Load(); at != 0 {
				now.Store(at)
			}
			h.ServeHTTP(w, r)
		})
	}
	bankHandler, err := simulator.BankWithKeys(ctx, bank, "read", "instruct", "post")
	if err != nil {
		t.Fatal(err)
	}
	bankHTTP := httptest.NewServer(audit(bankHandler, "/bank/settlement-timing"))
	defer bankHTTP.Close()
	aq := httptest.NewUnstartedServer(nil)
	processorHandler, err := simulator.AcquirerWithKeys(ctx, processor, "http://"+aq.Listener.Addr().String(), bankHTTP.URL, "read", "instruct", "approve")
	if err != nil {
		t.Fatal(err)
	}
	aq.Config.Handler = audit(processorHandler, "/sim/settlement-timing")
	aq.Start()
	defer aq.Close()
	exec := func(db *pgxpool.Pool, sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	payout := func(trace string, amt int64, at string) {
		exec(processor, `INSERT INTO sim_payout(id,operation_id,bank_trace,merchant_id,currency,batch,net,created_at)
				VALUES($1,$2,$3,'mimi','USD','standard',$4,$5::timestamptz)`, "po-"+trace, "ds-"+trace, "bt-"+trace, amt, day+at+"Z")
	}
	post := func(trace string, amt int64, at string) {
		exec(bank, `INSERT INTO sim_bank_entry(trace,merchant_id,currency,amount,received_at,posted_at)
				VALUES($1,'mimi','USD',$2,$3::timestamptz - interval '1 minute',$3::timestamptz)`, "bt-"+trace, amt, day+at+"Z")
	}
	setTime := func(wall string) {
		t.Helper()
		stamp, err := time.Parse(time.RFC3339Nano, day+wall+"Z")
		if err != nil {
			t.Fatal(err)
		}
		now.Store(stamp.UnixMicro())
	}
	setTime("12:00:00")
	start := func() http.Handler {
		t.Helper()
		s, err := New(ctx, merchant, aq.URL, bankHTTP.URL, "read")
		if err != nil {
			t.Fatal(err)
		}
		s.timingNow = func() time.Time { return time.UnixMicro(now.Load()).UTC() }
		return s.Handler()
	}
	h := start()
	// These booked records predate this reporting feature. They must keep
	// their identities, amounts, and booking times throughout every refresh.
	payout("control", 98, "08:00:00")
	post("control", 98, "08:05:00")
	exec(merchant, `INSERT INTO pay.settlement_requests(request_key,operation_id,batch) VALUES('control','ds-control','standard');
		INSERT INTO pay.payouts(id,operation_id,bank_trace,currency,net,status,bank_evidence,created_at)
		VALUES('po-control','ds-control','bt-control','USD',98,'matched','{"trace":"bt-control","merchant_id":"mimi","currency":"USD","amount":98,"status":"posted"}','2026-10-07T08:01Z');
		INSERT INTO pay.events(subject,kind,message,created_at) VALUES('po-control','matched','Verified deposit','2026-10-07T08:06Z')`)
	payout("grocery", 2940, "09:30:00")
	post("debit", -300, "10:10:00")
	const base = "/v1/settlement-timing"
	const noon = day + "12:00:00Z"
	const late = day + "14:00:00Z"
	ask := func(method, path string, body any, want int) obj {
		t.Helper()
		code, result := issue(h, method, path, "", body)
		if code != want {
			t.Fatalf("%s %s: %d %v; want %d", method, path, code, result, want)
		}
		return result
	}
	historyPath := func(effective, known string) string {
		return base + "/history?" + url.Values{"as_of": {effective}, "known_as_of": {known}}.Encode()
	}
	history := func(effective, known string) obj {
		t.Helper()
		before := calls.Load()
		r := ask("GET", historyPath(effective, known), nil, 200)
		if before != calls.Load() {
			t.Fatal("historical read contacted a live source")
		}
		return r
	}
	// Include exact serialization in the replay proof, not just derived totals.
	rawHistory := func(path string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("history %s: %d %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	row := func(r obj, section, trace string) obj {
		t.Helper()
		for _, item := range r[section].([]any) {
			line := item.(map[string]any)
			if line["bank_trace"] == "bt-"+trace {
				return line
			}
		}
		t.Fatalf("missing trace %s in %s: %v", trace, section, r)
		return nil
	}
	want := func(record, expected obj) {
		t.Helper()
		for k, v := range expected {
			if record[k] != v {
				t.Fatalf("%s=%v, want %v: %v", k, record[k], v, record)
			}
		}
	}
	count := func(r obj, open, matched int) {
		t.Helper()
		if len(r["open"].([]any)) != open || len(r["matched"].([]any)) != matched {
			t.Fatalf("want %d open, %d matched: %v", open, matched, r)
		}
	}
	payRecords := func() map[string]string {
		t.Helper()
		all := feeLedgerFingerprint(t, merchant, "pay")
		delete(all, "timing_observations")
		delete(all, "timing_facts")
		return all
	}
	financial := payRecords()
	sourceRecords := func() []map[string]string {
		return []map[string]string{feeLedgerFingerprint(t, processor, "public"), feeLedgerFingerprint(t, bank, "public")}
	}
	originalSources := sourceRecords()
	// A caller cannot claim an earlier time by putting it in the request.
	ask("POST", base+"/observations?as_of="+noon, nil, 422)
	ask("POST", base+"/observations", obj{"known_as_of": noon}, 422)
	ask("GET", historyPath(noon, noon), nil, 409)
	if calls.Load() != 0 {
		t.Fatal("invalid input or missing archive queried live sources")
	}
	first := ask("POST", base+"/observations", nil, 200)
	want(first, obj{"as_of": noon, "known_as_of": noon, "source_through": noon, "observed_at": noon})
	count(first, 2, 1)
	want(row(first, "open", "grocery"), obj{"payout_at": day + "09:30:00Z", "payout_known_at": noon, "status": "in_transit", "age_seconds": 9000.0, "posted_at": nil, "bank_known_at": nil})
	want(row(first, "open", "debit"), obj{"bank_amount": -300.0, "posted_at": day + "10:10:00Z", "bank_known_at": noon, "payout_known_at": nil, "status": "bank_only"})
	want(row(first, "matched", "control"), obj{"booked_match_at": day + "08:06:00Z", "booking_known_at": noon})
	if !reflect.DeepEqual(history(noon, noon), first) {
		t.Fatal("capture did not return its exact archive view")
	}
	rawOriginal := rawHistory(historyPath(noon, noon))
	archiveBefore := feeLedgerFingerprint(t, merchant, "pay")
	// Repeated capture at identical clocks is idempotent, including the
	// first-known time. A capture blocked by a source outage is atomic.
	if again := ask("POST", base+"/observations", obj{}, 200); !reflect.DeepEqual(first, again) {
		t.Fatal("repeat capture changed first observation")
	}
	setTime("13:00:00")
	unavailable.Store(true)
	ask("POST", base+"/observations", nil, 502)
	unavailable.Store(false)
	if !reflect.DeepEqual(archiveBefore, feeLedgerFingerprint(t, merchant, "pay")) || !reflect.DeepEqual(originalSources, sourceRecords()) {
		t.Fatal("duplicate capture or unavailable feed changed history")
	}
	// The independent sources now deliver older-dated facts in opposite
	// arrival order. These match the original signed USD amounts and traces.
	post("grocery", 2940, "09:40:00")
	payout("debit", -300, "09:15:00")
	afterDelivery := sourceRecords()
	current := ask("GET", base+"?as_of="+noon, nil, 200)
	count(current, 0, 3)
	want(row(current, "matched", "grocery"), obj{"posted_at": day + "09:40:00Z", "payout_at": day + "09:30:00Z", "age_seconds": 600.0})
	if rawHistory(historyPath(noon, noon)) != rawOriginal {
		t.Fatal("live effective-time query revised the archived report")
	}
	past := history(noon, day+"13:00:00Z")
	want(past, obj{"observed_at": noon, "source_through": noon})
	count(past, 2, 1)
	contradictory.Store(true)
	ask("POST", base+"/observations", nil, 409)
	contradictory.Store(false)
	if !reflect.DeepEqual(archiveBefore, feeLedgerFingerprint(t, merchant, "pay")) {
		t.Fatal("source GET or conflicting capture revised report evidence")
	}
	// Last delivery finishes two minutes after the recorded source cutoff.
	// Finance cannot treat the polling start time as first knowledge.
	setTime("14:00:00")
	deliveredAt.Store(now.Load())
	setTime("13:58:00")
	second := ask("POST", base+"/observations", nil, 200)
	deliveredAt.Store(0)
	count(second, 0, 3)
	want(second, obj{"as_of": day + "13:58:00Z", "known_as_of": late, "source_through": day + "13:58:00Z", "observed_at": late})
	corrected := history(noon, late)
	want(corrected, obj{"as_of": noon, "known_as_of": late, "source_through": day + "13:58:00Z", "observed_at": late})
	want(row(corrected, "matched", "grocery"), obj{"payout_known_at": noon, "posted_at": day + "09:40:00Z", "bank_known_at": late, "age_seconds": 600.0, "booked_match_at": nil})
	want(row(corrected, "matched", "debit"), obj{"payout_at": day + "09:15:00Z", "payout_known_at": late, "bank_known_at": noon, "age_seconds": 3300.0, "processor_amount": -300.0})
	count(history(day+"09:35:00Z", late), 2, 1) // Bank posting at 09:40 and debit at 10:10 are not yet effective.
	want(row(history(noon, day+"13:59:59.999999Z"), "open", "grocery"), obj{"bank_known_at": nil, "payout_known_at": noon})
	ask("GET", historyPath(noon, day+"11:59:59.999999Z"), nil, 422) // effective cutoff exceeds knowledge
	ask("GET", historyPath(day+"11:59:59.999999Z", day+"11:59:59.999999Z"), nil, 409)
	if rawHistory(historyPath(noon, noon)) != rawOriginal {
		t.Fatal("backdated independent deliveries revised known-at-noon bytes")
	}
	rawCorrected := rawHistory(historyPath(noon, late))
	final := feeLedgerFingerprint(t, merchant, "pay")
	h = start()
	unavailable.Store(true)
	beforeReplay := calls.Load()
	if rawHistory(historyPath(noon, noon)) != rawOriginal || rawHistory(historyPath(noon, late)) != rawCorrected || beforeReplay != calls.Load() {
		t.Fatal("restart or source outage changed archived results")
	}
	if !reflect.DeepEqual(final, feeLedgerFingerprint(t, merchant, "pay")) || !reflect.DeepEqual(financial, payRecords()) || !reflect.DeepEqual(afterDelivery, sourceRecords()) {
		t.Fatal("history reads, capture or restart changed original financial or retained reporting records")
	}
	t.Logf("Noon $29.40 payout stayed open as known then; 09:40 source posting first learned at 14:00; two backdated counterparts (including -$3.00) found. %d pre-existing financial/operational tables preserved.", len(financial)+len(afterDelivery[0])+len(afterDelivery[1]))
}

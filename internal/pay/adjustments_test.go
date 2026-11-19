package pay

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dakimpay/internal/simulator"
)

// The merchant reservation wins locally, but the provider-side refund wins
// at the source. Its callback is withheld until after remittance and restart.
func TestMissingRefundNoticeAndLostMerchantRefusal(t *testing.T) {
	ctx := t.Context()
	store, source, bankStore := isolatedPool(t), isolatedPool(t), isolatedPool(t)
	bankHandler, err := simulator.BankWithKeys(ctx, bankStore, "inspect", "instruct", "post")
	if err != nil {
		t.Fatal(err)
	}
	bank := httptest.NewServer(bankHandler)
	defer bank.Close()
	acquirer := httptest.NewUnstartedServer(nil)
	handler, err := simulator.AcquirerWithKeys(ctx, source, "http://"+acquirer.Listener.Addr().String(), bank.URL, "inspect", "instruct", "approve")
	if err != nil {
		t.Fatal(err)
	}
	parked, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	var lose atomic.Bool
	var contradictoryRefusal atomic.Bool
	acquirer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/sim/refunds/") && contradictoryRefusal.Load() {
			observed := httptest.NewRecorder()
			handler.ServeHTTP(observed, r)
			w.WriteHeader(observed.Code)
			raw := bytes.TrimSpace(observed.Body.Bytes())
			if observed.Code == http.StatusOK {
				raw = append([]byte(`{"status":"succeeded",`), raw[1:]...)
			}
			_, _ = w.Write(raw)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/sim/refunds" && lose.CompareAndSwap(true, false) {
			close(parked)
			select {
			case <-resume:
			case <-r.Context().Done():
				return
			}
			received := httptest.NewRecorder()
			handler.ServeHTTP(received, r)
			if received.Code != 409 {
				t.Errorf("expected a durable insufficient-balance refusal, got %d", received.Code)
			}
			panic(http.ErrAbortHandler)
		}
		handler.ServeHTTP(w, r)
	})
	acquirer.Start()
	defer acquirer.Close()
	defer unblock()
	start := func() *Service {
		s, err := New(ctx, store, acquirer.URL, bank.URL, "inspect")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := start()
	call := func(method, path, key string, body any, expected int) obj {
		t.Helper()
		code, out := issue(s.Handler(), method, path, key, body)
		if code != expected {
			t.Fatalf("%s %s: %d, want %d: %v", method, path, code, expected, out)
		}
		return out
	}
	postBank := func(payout obj) {
		t.Helper()
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
		if res.StatusCode != 200 {
			t.Fatal(res.StatusCode)
		}
		if matched := call("POST", "/v1/reconcile", "", nil, 200); matched["matched"] != float64(1) {
			t.Fatal(matched)
		}
	}
	p := call("POST", "/v1/payments", "sale-key", PaymentInput{"MIMI-sale", "terminal", 1000, "USD", "visa", "MIMI-01", false}, 201)
	first := call("POST", "/v1/settle", "original", nil, 201)
	postBank(first)
	firstURL := "/v1/payouts/" + first["id"].(string) + "/statement"
	before := call("GET", firstURL, "", nil, 200)
	if before["depositMinor"] != float64(980) {
		t.Fatal(before)
	}
	path := "/v1/payments/" + p["id"].(string)
	balance := func(succeeded, pending, reserved, available int, review bool) {
		t.Helper()
		view := call("GET", path, "", nil, 200)
		if view["refunded"] != float64(succeeded) || view["refund_pending_requested"] != float64(pending) ||
			view["refund_reserved"] != float64(reserved) || view["refundable"] != float64(available) || view["refund_balance_review"] != review {
			t.Fatalf("unexpected merchant refund balance: %+v", view)
		}
	}
	lose.Store(true)
	type result struct {
		status int
		data   obj
	}
	merchant := make(chan result, 1)
	go func() {
		status, body := issue(s.Handler(), "POST", path+"/refunds", "competing", obj{"amount": 800})
		merchant <- result{status, body}
	}()
	select {
	case <-parked:
	case <-time.After(8 * time.Second):
		t.Fatal("merchant refund dispatch did not reach the source")
	}
	f, err := simulator.RecordSourceRefund(ctx, source, "sr_later", p["reference"].(string), "mimi", "USD", 700)
	if err != nil {
		t.Fatal(err)
	}
	unblock()
	lost := <-merchant
	if lost.status != 202 || lost.data["status"] != "unknown" {
		t.Fatal(lost)
	}
	refundPath := "/v1/refunds/" + lost.data["id"].(string)
	feed := "/v1/adjustments?after=0&limit=1"
	if page := call("GET", feed, "", nil, 200); len(page["adjustments"].([]any)) != 0 {
		t.Fatal(page)
	}
	s = start()
	// The statement is the first notice of a real $7 source refund. The
	// original merchant $8 is still unresolved on a $10 capture.
	call("POST", "/v1/settle", "debit", nil, 409)
	assertParked := func() {
		t.Helper()
		var held, reserved, sourceRefunds, payouts int
		err := store.QueryRow(ctx, `SELECT
		    (SELECT count(*) FROM pay.refund_source_holds WHERE source_operation=$1 AND reference=$2 AND payment_id=$3
		      AND amount=700 AND currency='USD' AND charge_reference=$4 AND original_payout=$5 AND occurred_at=$6),
		    (SELECT coalesce(sum(amount),0) FROM pay.refunds WHERE payment_id=$3 AND status='unknown'),
		    (SELECT count(*) FROM pay.refunds WHERE source_operation IS NOT NULL),
		    (SELECT count(*) FROM pay.payouts)`,
			f.Operation, f.Reference, p["id"], p["reference"], first["id"], f.OccurredAt).Scan(&held, &reserved, &sourceRefunds, &payouts)
		if err != nil || held != 1 || reserved != 800 || sourceRefunds != 0 || payouts != 1 {
			t.Fatal("parked source, original reservation, published sources and local payouts", held, reserved, sourceRefunds, payouts, err)
		}
		unpublished := call("GET", feed, "", nil, 200)
		if unpublished["next_cursor"] != float64(0) || len(unpublished["adjustments"].([]any)) != 0 {
			t.Fatal("unresolved source was exported to Mimi", unpublished)
		}
		balance(0, 800, 800, 0, true)
	}
	assertParked()
	s = start()
	call("POST", "/v1/settle", "debit", nil, 409)
	raw := envelope(t, "delayed", "sr_later", "refund.observed")
	for _, want := range []int{202, 200} {
		if got := callbackRequest(s.CallbackHandler(signingFixture), raw, signingFixture, time.Now()); got != want {
			t.Fatal(got)
		}
	}
	if err := s.ProcessCallbacks(ctx); err != nil {
		t.Fatal(err)
	}
	var delivery string
	if err = store.QueryRow(ctx, "SELECT status FROM pay.processor_callbacks WHERE event_id='delayed'").Scan(&delivery); err != nil || delivery != "pending" {
		t.Fatal("ambiguity acknowledged as a posted refund", delivery, err)
	}
	changed := sourceRefund{refundFact: refundFact{refundRequest{f.Operation, "mimi", f.Charge, 699, "USD"}, f.Reference, "succeeded"},
		PaymentID: f.PaymentID, Origin: "acquirer", OccurredAt: f.OccurredAt}
	if status, _, err := s.recordSourceRefund(ctx, changed); err != nil || status != "exception" {
		t.Fatal("altered source terms replaced parked evidence", status, err)
	}
	changed.Amount, changed.OperationID = 700, "conflicting-operation"
	if status, _, err := s.recordSourceRefund(ctx, changed); err != nil || status != "exception" {
		t.Fatal("parked source reference reassigned to another operation", status, err)
	}
	changed.Amount, changed.Reference = 301, "another-reference"
	if status, _, err := s.recordSourceRefund(ctx, changed); err != nil || status != "exception" {
		t.Fatal("verified sources exceeded the original capture", status, err)
	}
	call("POST", path+"/refunds", "one-cent", obj{"amount": 1}, 409)
	contradictoryRefusal.Store(true)
	call("POST", refundPath+"/recover", "", nil, 502)
	if held := call("GET", refundPath, "", nil, 200); held["status"] != "unknown" {
		t.Fatal("ambiguous source refusal released the reservation", held)
	}
	assertParked()
	contradictoryRefusal.Store(false)
	refused := call("POST", refundPath+"/recover", "", nil, 200)
	if refused["status"] != "declined" {
		t.Fatal(refused)
	}
	balance(0, 0, 0, 0, true)
	// Interrupt promotion between ledger insertion and hold removal. The
	// posted amount, cursor and held evidence must recover as one transaction.
	if _, err = store.Exec(ctx, `CREATE FUNCTION fail_promotion() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'synthetic write interruption'; END $$;
		CREATE TRIGGER fail_promotion BEFORE DELETE ON pay.refund_source_holds
		FOR EACH ROW EXECUTE FUNCTION fail_promotion()`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.inspectSourceRefund(ctx, f.Operation, ""); err == nil {
		t.Fatal("failed hold removal escaped the financial transaction")
	}
	if held := call("GET", feed, "", nil, 200); len(held["adjustments"].([]any)) != 0 {
		t.Fatal("partially promoted hold was exposed to Mimi", held)
	}
	var retained bool
	if err = store.QueryRow(ctx, "SELECT EXISTS(SELECT FROM pay.refund_source_holds WHERE source_operation=$1 AND amount=700)", f.Operation).Scan(&retained); err != nil || !retained {
		t.Fatal("rollback lost the verified provider movement", retained, err)
	}
	if _, err = store.Exec(ctx, "DROP TRIGGER fail_promotion ON pay.refund_source_holds"); err != nil {
		t.Fatal(err)
	}
	// There is no merchant retry, new settlement or incoming callback here.
	// Restart's regular recovery must revisit and independently verify the
	// parked movement before publishing it to Mimi.
	s = start()
	if err = s.recoverDue(ctx); err != nil {
		t.Fatal(err)
	}
	page := call("GET", feed, "", nil, 200)
	adjustments := page["adjustments"].([]any)
	if len(adjustments) != 1 {
		t.Fatal("expected exactly one published source refund", page)
	}
	adjustment := adjustments[0].(map[string]any)
	if adjustment["original_payout"] != first["id"] || adjustment["original_bank"] != before["bank"] || adjustment["original_deposit_minor"] != before["depositMinor"] ||
		adjustment["reference"] != f.Reference || adjustment["payment_id"] != p["id"] || adjustment["amount"] != float64(700) {
		t.Fatal(adjustment)
	}
	var holds int
	if err = store.QueryRow(ctx, "SELECT count(*) FROM pay.refund_source_holds").Scan(&holds); err != nil || holds != 0 {
		t.Fatal("promoted source still reserved", holds, err)
	}
	balance(700, 0, 0, 300, false)
	later := call("POST", "/v1/settle", "debit", nil, 201)
	if later["net"] != float64(-700) || later["id"] == first["id"] || later["bank_trace"] == first["bank_trace"] {
		t.Fatal(later)
	}
	laterURL := "/v1/payouts/" + later["id"].(string) + "/statement"
	call("GET", laterURL, "", nil, 409)
	if _, err = store.Exec(ctx, "UPDATE pay.processor_callbacks SET next_attempt=now() WHERE event_id='delayed'"); err != nil {
		t.Fatal(err)
	}
	if err := s.ProcessCallbacks(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(page, call("GET", feed, "", nil, 200)) {
		t.Fatal("notice duplicated the source adjustment")
	}
	if err = store.QueryRow(ctx, "SELECT status FROM pay.processor_callbacks WHERE event_id='delayed'").Scan(&delivery); err != nil || delivery != "applied" {
		t.Fatal("original notice failed to converge after authoritative inquiry", delivery, err)
	}
	if got := call("GET", path+"/reversal-evidence", "", nil, 200); got["complete"] != true || len(got["refunds"].([]any)) != 1 {
		t.Fatal(got)
	}
	postBank(later)
	after := call("GET", laterURL, "", nil, 200)
	wantLine := obj{"reference": adjustment["id"], "grossMinor": float64(-700), "feeMinor": float64(0), "kind": "provider_refund", "original": p["id"]}
	if !reflect.DeepEqual(after["lines"], []any{wantLine}) || after["depositMinor"] != float64(-700) {
		t.Fatal(after)
	}
	s = start()
	if !reflect.DeepEqual(before, call("GET", firstURL, "", nil, 200)) || !reflect.DeepEqual(after, call("GET", laterURL, "", nil, 200)) {
		t.Fatal("restart changed matched statements")
	}
	if replay := call("POST", "/v1/settle", "debit", nil, 200); replay["id"] != later["id"] {
		t.Fatal(replay)
	}
	if replay := call("POST", "/v1/reconcile", "", nil, 200); replay["matched"] != float64(0) {
		t.Fatal(replay)
	}
	remaining := call("POST", path+"/refunds", "remainder", obj{"amount": 300}, 201)
	if remaining["status"] != "succeeded" {
		t.Fatal(remaining)
	}
	call("POST", path+"/refunds", "over", obj{"amount": 1}, 409)
	balance(1000, 0, 0, 0, false)
}

func TestAdjustmentFeedCommitOrderAndConflictingReplay(t *testing.T) {
	ctx := t.Context()
	db := isolatedPool(t)
	s, err := New(ctx, db, "http://127.0.0.1", "http://127.0.0.1", "local-only")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `INSERT INTO pay.settlement_requests(request_key,operation_id,batch) VALUES('key','ds_orig','standard');
	INSERT INTO pay.payouts(id,operation_id,bank_trace,currency,net,status) VALUES('po_orig','ds_orig','bt_orig','USD',196,'in_transit');
	INSERT INTO pay.payments(id,merchant_id,request_key,order_id,channel,amount,currency,brand,terminal_id,lose_response,status,reference)
	SELECT 'dp_'||i,'mimi','order'||i,'MIMI-'||i,'terminal',100,'USD','visa','MIMI-01',false,'captured','ch_'||i FROM generate_series(1,2) i;
	INSERT INTO pay.lines(id,payout_id,kind,reference,amount) SELECT 'sl_'||i,'po_orig','capture','ch_'||i,100 FROM generate_series(1,2) i;
	CREATE FUNCTION delay_first_adjustment() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(533521); RETURN NEW; END $$;
	CREATE TRIGGER delay_first_adjustment BEFORE INSERT ON pay.refunds FOR EACH ROW WHEN (NEW.source_operation='sr_1') EXECUTE FUNCTION delay_first_adjustment()`)
	if err != nil {
		t.Fatal(err)
	}
	hold, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx)
	if _, err = hold.Exec(ctx, "SELECT pg_advisory_xact_lock(533521)"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	fact := func(i string) sourceRefund {
		request := refundRequest{OperationID: "sr_" + i, MerchantID: "mimi", ChargeReference: "ch_" + i, Amount: 50, Currency: "USD"}
		return sourceRefund{refundFact: refundFact{refundRequest: request, Reference: "rf_" + i, Status: "succeeded"},
			PaymentID: "dp_" + i, Origin: "acquirer", OccurredAt: now}
	}
	if state, _, err := s.recordSourceRefund(ctx, fact("2")); err != nil || state != "pending" {
		t.Fatal("provider payout alone made a finance adjustment eligible", state, err)
	}
	if matched, err := s.matchBank(ctx, Payout{ID: "po_orig"}, bankEntry{"bt_orig", "mimi", "USD", 196, "posted"}); err != nil || !matched {
		t.Fatal(matched, err)
	}
	waitBlocked := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var n int
			if err := db.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND locktype='advisory' AND NOT granted").Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("expected %d blocked advisory-lock acquisitions", want)
	}
	done := make(chan error, 2)
	ingest := func(i string) {
		status, detail, err := s.recordSourceRefund(ctx, fact(i))
		if err == nil && status != "applied" {
			err = errors.New(detail)
		}
		done <- err
	}
	go ingest("1")
	waitBlocked(1) // first writer already allocated a sequence, but has not committed
	go ingest("2")
	waitBlocked(2) // distinct capture, blocked by the merchant feed's ordering lock
	page := func(query string) obj {
		t.Helper()
		code, p := issue(s.Handler(), "GET", "/v1/adjustments"+query, "", nil)
		if code != 200 {
			t.Fatal(p)
		}
		return p
	}
	if got := page("?after=0"); got["next_cursor"] != float64(0) || len(got["adjustments"].([]any)) != 0 {
		t.Fatal(got)
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	first := page("?limit=1")
	second := page("?after=1&limit=1")
	empty := page("?after=2")
	if first["next_cursor"] != float64(1) || second["next_cursor"] != float64(2) || empty["next_cursor"] != float64(2) || len(empty["adjustments"].([]any)) != 0 ||
		first["adjustments"].([]any)[0].(map[string]any)["source_operation"] != "sr_1" || second["adjustments"].([]any)[0].(map[string]any)["source_operation"] != "sr_2" {
		t.Fatal(first, second, empty)
	}
	altered := fact("1")
	altered.Amount--
	if status, _, err := s.recordSourceRefund(ctx, altered); err != nil || status != "exception" {
		t.Fatal(status, err)
	}
	if !reflect.DeepEqual(page("?limit=1"), first) {
		t.Fatal("conflicting source replay changed original record")
	}
	// An adapter for another processor can verify a statement movement with
	// different operation and refund reference formats using the same ledger.
	other := fact("2")
	other.OperationID, other.Reference = "NETWORK-72", "RETURN-418"
	s.lookupRefund = func(_ context.Context, operation, reference string) (sourceRefund, bool, error) {
		return other, operation == other.OperationID || reference == other.Reference, nil
	}
	for _, key := range [][2]string{{"", other.Reference}, {other.OperationID, ""}} {
		if status, _, err := s.inspectSourceRefund(ctx, key[0], key[1]); err != nil || status != "applied" {
			t.Fatal("alternate processor lookup", status, err)
		}
	}
	next := page("?after=2")
	rows := next["adjustments"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["source_operation"] != other.OperationID {
		t.Fatal("alternate processor replay duplicated or lost the adjustment", next)
	}
}

func TestHeldSourceRecoveryBoundedAcrossMissingAndConflictingEvidence(t *testing.T) {
	ctx, db := t.Context(), isolatedPool(t)
	s, err := New(ctx, db, "http://127.0.0.1", "http://127.0.0.1", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `INSERT INTO pay.settlement_requests(request_key,operation_id,batch) VALUES('seed','orig','standard');
	  INSERT INTO pay.payouts(id,operation_id,bank_trace,currency,net,status,bank_evidence)
	      VALUES('payout','orig','bank','USD',98,'matched','{"status":"posted"}');
	  INSERT INTO pay.payments(id,merchant_id,request_key,order_id,channel,amount,currency,brand,terminal_id,lose_response,status,reference)
	      VALUES('payment','mimi','order','MIMI-order','terminal',100,'USD','visa','MIMI-01',false,'captured','capture');
	  INSERT INTO pay.lines(id,payout_id,kind,reference,amount) VALUES('line','payout','capture','capture',100);
	  INSERT INTO pay.refund_source_holds(source_operation,reference,payment_id,amount,currency,charge_reference,original_payout,occurred_at,next_check)
	      SELECT 'source-'||i,'reference-'||i,'payment',1,'USD','capture','payout','2020-01-01 UTC',now()-interval '1 day'
	      FROM generate_series(1,6) i;`)
	if err != nil {
		t.Fatal(err)
	}
	var queried []string
	s.lookupRefund = func(_ context.Context, operation, reference string) (sourceRefund, bool, error) {
		queried = append(queried, operation+"/"+reference)
		// A contradictory lookup also stays on hold and yields to other cases.
		return sourceRefund{}, operation == "source-2", nil
	}
	for _, expected := range []int{4, 6, 6} {
		if err = s.recoverHeldRefunds(ctx); err != nil || len(queried) != expected {
			t.Fatalf("queried %v; expected %d distinct due holds: %v", queried, expected, err)
		}
	}
	want := []string{"source-1/reference-1", "source-2/reference-2", "source-3/reference-3", "source-4/reference-4", "source-5/reference-5", "source-6/reference-6"}
	if !reflect.DeepEqual(queried, want) {
		t.Fatal("recovery changed the original operation or skipped a due case", queried)
	}
	var held, delayed, published int
	if err = db.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE next_check>now()+interval '4 minutes'),
		(SELECT count(*) FROM pay.refunds) FROM pay.refund_source_holds WHERE last_message<>''`).Scan(&held, &delayed, &published); err != nil || held != 6 || delayed != 1 || published != 0 {
		t.Fatal("unavailable or conflicting evidence released a hold", held, delayed, published, err)
	}
	// A frozen statement retry need not wait for the scheduled check. It asks
	// the source by the original line reference and can publish only that hold.
	f := sourceRefund{refundFact: refundFact{refundRequest{"source-6", "mimi", "capture", 1, "USD"}, "reference-6", "succeeded"},
		PaymentID: "payment", Origin: "acquirer", OccurredAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	s.lookupRefund = func(_ context.Context, operation, reference string) (sourceRefund, bool, error) {
		return f, operation == "" && reference == f.Reference, nil
	}
	for range 2 {
		if err = s.discoverStatementRefunds(ctx, []Line{{Kind: "refund", Reference: f.Reference}}); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.QueryRow(ctx, `SELECT (SELECT count(*) FROM pay.refund_source_holds),
		(SELECT count(*) FROM pay.refunds WHERE source_operation='source-6' AND amount=1 AND status='succeeded')`).Scan(&held, &published); err != nil || held != 5 || published != 1 {
		t.Fatal("statement did not promote exactly one original source", held, published, err)
	}
}

package simulator

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"dakimpay/internal/processor"
)

func TestSourceRefundAfterDeposit(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()
	acquirer, bank := store.acquirer.URL, store.bank.URL
	var c charge
	input := chargeRequest{Operation: "dp_prior", Order: "MIMI-RECEIPT", Merchant: merchant, Currency: currency, Amount: 1000, Brand: "visa", Channel: "terminal", Terminal: "MIMI-01"}
	call(t, "POST", acquirer+"/sim/charges", serviceKey, input, 200, &c)
	makeRefund := func(amount int64) (SourceRefund, error) {
		return RecordSourceRefund(ctx, store.aq, "sr_late", c.Reference, merchant, currency, amount)
	}
	if _, err := makeRefund(300); err == nil {
		t.Fatal("source adjustment was eligible before the first payout")
	}
	batch := payoutRequest{Operation: "first", Merchant: merchant, Currency: currency, Batch: "standard"}
	var first payout
	call(t, "POST", acquirer+"/sim/payouts", serviceKey, batch, 200, &first)
	var credit bankEntry
	call(t, "POST", bank+"/bank/post/"+first.Trace, postKey, nil, 200, &credit)
	if credit.Status != "posted" || credit.Amount != 980 || credit.Trace != first.Trace {
		t.Fatal("original bank posting", credit)
	}

	const workers = 12
	var wg sync.WaitGroup
	results := make([]SourceRefund, workers)
	errs := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = makeRefund(300)
		}()
	}
	wg.Wait()
	fact := results[0]
	for i, got := range results {
		if errs[i] != nil || got != fact {
			t.Fatalf("source replay %d: %v %v", i, got, errs[i])
		}
	}
	if fact.Reference == "" || fact.OccurredAt.IsZero() || fact.Origin != "acquirer" || fact.Status != "succeeded" ||
		fact.Amount != 300 || fact.PaymentID != c.Operation || fact.Charge != c.Reference {
		t.Fatal("incomplete source observation", fact)
	}
	if _, err := makeRefund(301); err == nil {
		t.Fatal("source operation accepted different terms")
	}
	call(t, "POST", acquirer+"/sim/refunds", serviceKey, fact.refundRequest, 409, nil)
	count(t, store.aq, "SELECT count(*) FROM sim_refund", 1)
	count(t, store.aq, "SELECT count(*) FROM sim_line WHERE kind='refund' AND amount=-300 AND payout_id IS NULL", 1)
	count(t, store.aq, "SELECT count(*) FROM sim_callback_outbox WHERE kind='refund.observed' AND operation_id='sr_late'", 1)
	count(t, store.bk, "SELECT count(*) FROM sim_bank_entry", 1)
	observed := make(chan processor.Event, 2)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		var ev processor.Event
		if err != nil || processor.Verify(r.Header, raw, callbackTestKey, time.Now()) != nil || json.Unmarshal(raw, &ev) != nil {
			http.Error(w, "invalid notification", 401)
			return
		}
		observed <- ev
		jsonResponse(w, http.StatusAccepted, map[string]string{"event_id": ev.ID})
	}))
	t.Cleanup(receiver.Close)
	for range 2 {
		if delivered, err := DeliverCallback(ctx, store.aq, receiver.URL, callbackTestKey); !delivered || err != nil {
			t.Fatal("notification delivery", delivered, err)
		}
	}
	<-observed // Original capture.
	if notice := <-observed; notice.Type != "refund.observed" || notice.OperationID != fact.Operation || notice.MerchantID != merchant {
		t.Fatal("notification does not identify the posted source refund", notice)
	}
	var e map[string]any
	call(t, "GET", acquirer+"/sim/charges/"+c.Operation+"/reversal-evidence", serviceKey, nil, 200, &e)
	refunds := e["refunds"].([]any)
	if len(refunds) != 1 || refunds[0].(map[string]any)["operation_id"] != fact.Operation {
		t.Fatal("source refund absent from original capture history", e)
	}
	prior, err := AcquirerWithKeys(ctx, store.aq, acquirer, bank, serviceKey, instructKey, approveKey)
	if err != nil {
		t.Fatal(err)
	}
	restarted := httptest.NewServer(prior)
	t.Cleanup(restarted.Close)
	base := restarted.URL + "/sim/source-refunds/"
	for _, path := range []string{fact.Operation, "by-reference/" + fact.Reference} {
		var recovered SourceRefund
		raw := call(t, "GET", base+path, serviceKey, nil, 200, &recovered)
		var document map[string]json.RawMessage
		if json.Unmarshal(raw, &document) != nil || len(document) != 10 || recovered != fact {
			t.Fatal("source read changed financial terms", path, recovered)
		}
		call(t, "GET", base+path, approveKey, nil, 401, nil)
	}
	call(t, "GET", restarted.URL+"/sim/refunds/"+fact.Operation, serviceKey, nil, 404, nil)
	call(t, "GET", base+"by-reference/missing", serviceKey, nil, 404, nil)
	call(t, "POST", base+fact.Operation, serviceKey, nil, 405, nil)
	var unchanged payout
	call(t, "POST", restarted.URL+"/sim/payouts", serviceKey, batch, 200, &unchanged)
	if !reflect.DeepEqual(unchanged, first) {
		t.Fatal("first statement changed", unchanged)
	}
	batch.Operation = "later"
	var debit payout
	call(t, "POST", restarted.URL+"/sim/payouts", serviceKey, batch, 200, &debit)
	if debit.ID == first.ID || debit.Trace == first.Trace || debit.Net != -300 || len(debit.Lines) != 1 ||
		debit.Lines[0].Kind != "refund" || debit.Lines[0].Amount != -300 || debit.Lines[0].Reference != fact.Reference {
		t.Fatal("subsequent payout", debit)
	}
	call(t, "GET", bank+"/bank/entries/"+debit.Trace, serviceKey, nil, 404, nil)
	for range 2 {
		call(t, "POST", restarted.URL+"/sim/payouts", serviceKey, batch, 200, nil)
		call(t, "POST", bank+"/bank/post/"+debit.Trace, postKey, nil, 200, nil)
	}
	var paid bankEntry
	call(t, "GET", bank+"/bank/entries/"+debit.Trace, serviceKey, nil, 200, &paid)
	if paid.Trace != debit.Trace || paid.Status != "posted" || paid.Amount != -300 {
		t.Fatal(paid)
	}
	var original bankEntry
	call(t, "GET", bank+"/bank/entries/"+first.Trace, serviceKey, nil, 200, &original)
	if credit != original {
		t.Fatal("original bank entry changed", credit, original)
	}
	count(t, store.bk, "SELECT count(*) FROM sim_bank_entry WHERE posted_at IS NOT NULL", 2)
	count(t, store.aq, "SELECT count(*) FROM sim_refund", 1)
}

func TestSourceRefundMigrationAndCallbackRollback(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()
	var c charge
	call(t, "POST", store.acquirer.URL+"/sim/charges", serviceKey, chargeRequest{Operation: "dp_old", Order: "MIGRATION", Merchant: merchant, Currency: currency,
		Amount: 400, Brand: "visa", Channel: "terminal", Terminal: "MIMI-01"}, 200, &c)
	old := refundRequest{Operation: "sr_legacy_merchant", Charge: c.Reference, Amount: 25, Merchant: merchant, Currency: currency}
	var oldFact refund
	call(t, "POST", store.acquirer.URL+"/sim/refunds", serviceKey, old, 200, &oldFact)
	call(t, "POST", store.acquirer.URL+"/sim/payouts", serviceKey, payoutRequest{"old-payout", merchant, currency, "standard"}, 200, nil)
	// Recreate the structures from the previous checkpoint around the real
	// committed refund and callback. Origin cannot be inferred from a key prefix.
	if _, err := store.aq.Exec(ctx, `ALTER TABLE sim_refund DROP COLUMN origin;
 DROP TABLE sim_refund_decline;
 ALTER TABLE sim_callback_outbox DROP CONSTRAINT sim_callback_event_kind,
 ADD CONSTRAINT sim_callback_outbox_kind_check CHECK(kind IN ('payment.awaiting_customer','payment.captured','refund.succeeded'))`); err != nil {
		t.Fatal(err)
	}
	handler, err := AcquirerWithKeys(ctx, store.aq, store.acquirer.URL, store.bank.URL, serviceKey, instructKey, approveKey)
	if err != nil {
		t.Fatal(err)
	}
	upgraded := httptest.NewServer(handler)
	t.Cleanup(upgraded.Close)
	var migrated refund
	call(t, "GET", upgraded.URL+"/sim/refunds/"+old.Operation, serviceKey, nil, 200, &migrated)
	if migrated != oldFact {
		t.Fatal("migration changed original merchant refund", migrated)
	}
	call(t, "GET", upgraded.URL+"/sim/source-refunds/"+old.Operation, serviceKey, nil, 404, nil)
	if _, err = RecordSourceRefund(ctx, store.aq, old.Operation, c.Reference, merchant, currency, 25); err == nil {
		t.Fatal("source claimed a preexisting merchant refund")
	}
	if _, err = store.aq.Exec(ctx, "ALTER TABLE sim_callback_outbox ADD CONSTRAINT simulate_fault CHECK(kind<>'refund.observed')"); err != nil {
		t.Fatal(err)
	}
	const key = "sr_initially_blocked"
	if _, err = RecordSourceRefund(ctx, store.aq, key, c.Reference, merchant, currency, 375); err == nil {
		t.Fatal("missing source callback did not abort financial mutation")
	}
	call(t, "GET", upgraded.URL+"/sim/source-refunds/"+key, serviceKey, nil, 404, nil)
	count(t, store.aq, "SELECT count(*) FROM sim_line WHERE payout_id IS NULL", 0)
	if _, err = store.aq.Exec(ctx, "ALTER TABLE sim_callback_outbox DROP CONSTRAINT simulate_fault"); err != nil {
		t.Fatal(err)
	}
	fact, err := RecordSourceRefund(ctx, store.aq, key, c.Reference, merchant, currency, 375)
	if err != nil || fact.Amount != 375 {
		t.Fatal(err, fact)
	}
	count(t, store.aq, "SELECT count(*) FROM sim_line WHERE payout_id IS NULL", 1)
	count(t, store.aq, "SELECT count(*) FROM sim_callback_outbox WHERE kind='refund.observed'", 1)
}

func TestSourceAndMerchantRefundShareLimit(t *testing.T) {
	store := newTestStore(t)
	ctx, acquirer := t.Context(), store.acquirer.URL
	var captured charge
	call(t, "POST", acquirer+"/sim/charges", serviceKey, chargeRequest{Operation: "dp_race", Order: "RACE", Merchant: merchant,
		Currency: currency, Amount: 700, Brand: "amex", Channel: "terminal", Terminal: "MIMI-01"}, 200, &captured)
	call(t, "POST", acquirer+"/sim/payouts", serviceKey, payoutRequest{"original", merchant, currency, "standard"}, 200, nil)
	codes := make(chan int, 8)
	start := make(chan struct{})
	for i := range cap(codes) {
		go func() {
			<-start
			if i%2 == 0 {
				_, err := RecordSourceRefund(ctx, store.aq, fmt.Sprintf("sr_%d", i), captured.Reference, merchant, currency, 500)
				var f failure
				switch {
				case err == nil:
					codes <- http.StatusOK
				case errors.As(err, &f):
					codes <- f.status
				default:
					codes <- -1
				}
				return
			}
			op := refundRequest{Operation: fmt.Sprintf("dr_%d", i), Charge: captured.Reference, Amount: 500, Merchant: merchant, Currency: currency}
			code, _, _ := request("POST", acquirer+"/sim/refunds", serviceKey, op)
			codes <- code
		}()
	}
	close(start)
	got := map[int]int{}
	for range cap(codes) {
		got[<-codes]++
	}
	if got[200] != 1 || got[409] != 7 {
		t.Fatal("competing requests", got)
	}
	count(t, store.aq, "SELECT count(*) FROM sim_refund", 1)
	count(t, store.aq, "SELECT count(*) FROM sim_line WHERE kind='refund' AND amount=-500", 1)
	count(t, store.aq, "SELECT count(*) FROM sim_callback_outbox WHERE kind LIKE 'refund.%'", 1)
	op := refundRequest{Operation: "dr_no_balance", Charge: captured.Reference, Amount: 201, Merchant: merchant, Currency: currency}
	call(t, "POST", acquirer+"/sim/refunds", serviceKey, op, 409, nil)
	retry := op
	retry.Amount = 100
	call(t, "POST", acquirer+"/sim/refunds", serviceKey, retry, 409, nil)
	call(t, "POST", acquirer+"/sim/refunds", serviceKey, op, 409, nil)
	var denied refund
	call(t, "GET", acquirer+"/sim/refunds/"+op.Operation, serviceKey, nil, 200, &denied)
	if denied.refundRequest != op || denied.Status != "declined" || denied.Reference != "" {
		t.Fatal("rejected operation lacks original bound evidence", denied)
	}
	call(t, "GET", acquirer+"/sim/source-refunds/"+op.Operation, serviceKey, nil, 404, nil)
	const deniedSource = "sr_no_balance"
	for _, amount := range []int64{201, 100, 201} {
		if _, err := RecordSourceRefund(ctx, store.aq, deniedSource, captured.Reference, merchant, currency, amount); err == nil {
			t.Fatal("insufficient source adjustment was rebound", amount)
		}
	}
	call(t, "GET", acquirer+"/sim/refunds/"+deniedSource, serviceKey, nil, 404, nil)
	call(t, "GET", acquirer+"/sim/source-refunds/"+deniedSource, serviceKey, nil, 404, nil)
	count(t, store.aq, "SELECT count(*) FROM sim_refund WHERE operation_id='dr_no_balance'", 0)
	count(t, store.aq, "SELECT count(*) FROM sim_callback_outbox WHERE operation_id='dr_no_balance'", 0)
}

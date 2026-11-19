package simulator

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestSettlementTimingInventoriesUseNativeSourceTimes(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	cutoff := time.Date(2026, 6, 18, 12, 0, 0, 123456000, time.UTC)
	earlier := cutoff.Add(-25 * time.Hour)
	later := cutoff.Add(time.Microsecond)
	_, err := s.aq.Exec(ctx, `INSERT INTO sim_payout(operation_id,id,bank_trace,merchant_id,currency,batch,net,created_at) VALUES
		('op-a','payout-a','trace-a','mimi','USD','standard',505,$1),
		('op-c','payout-c','trace-c','mimi','USD','standard',0,$2),
		('op-b','payout-b','trace-b','mimi','USD','standard',-200,$2),
		('op-future','payout-future','trace-future','mimi','USD','standard',303,$3)`, earlier, cutoff, later)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.bk.Exec(ctx, `INSERT INTO sim_bank_entry(trace,merchant_id,currency,amount,received_at,posted_at) VALUES
		('trace-a','mimi','USD',505,$1,$3),
		('trace-z','mimi','USD',-600,$1,$2),
		('trace-d','mimi','USD',535,$1,$2),
		('instruction','mimi','USD',305,$1,NULL)`, earlier, cutoff, later)
	if err != nil {
		t.Fatal(err)
	}

	// Include PostgreSQL row versions to catch a same-value rewrite as well
	// as a changed payout, instruction or original timestamp.
	sourceState := func() [2]string {
		t.Helper()
		var fingerprints [2]string
		if err := s.aq.QueryRow(ctx, `SELECT md5(string_agg(p.xmin::text || row_to_json(p)::text,',' ORDER BY id)) FROM sim_payout p`).Scan(&fingerprints[0]); err != nil {
			t.Fatal(err)
		}
		if err := s.bk.QueryRow(ctx, `SELECT md5(string_agg(b.xmin::text || row_to_json(b)::text,',' ORDER BY trace)) FROM sim_bank_entry b`).Scan(&fingerprints[1]); err != nil {
			t.Fatal(err)
		}
		return fingerprints
	}
	beforeReads := sourceState()

	processorURL := s.acquirer.URL + "/sim/settlement-timing"
	bankURL := s.bank.URL + "/bank/settlement-timing"
	withCutoff := func(in time.Time) string { return "?as_of=" + url.QueryEscape(in.Format(time.RFC3339Nano)) }
	// Different time zones representing the same instant must select the
	// same rows. Equal timestamps are then ordered by immutable identity.
	query := withCutoff(cutoff.In(time.FixedZone("local", 8*3600)))
	for _, origin := range []string{processorURL, bankURL} {
		call(t, "GET", origin+query, "", nil, http.StatusUnauthorized, nil)
		call(t, "GET", origin+query, postKey, nil, http.StatusUnauthorized, nil)
		for _, params := range []string{"", "?as_of=", "?as_of=2026-06-18", query + "&as_of=2026-06-19", query + "&merchant_id=other", query + "&bad=abc%XX"} {
			call(t, "GET", origin+params, serviceKey, nil, http.StatusUnprocessableEntity, nil)
		}
	}
	var payouts timingInventory[payoutTimingRecord]
	base := call(t, "GET", processorURL+query, serviceKey, nil, http.StatusOK, &payouts)
	if !payouts.Complete || payouts.Merchant != "mimi" || payouts.Currency != "USD" || !payouts.AsOf.Equal(cutoff) || len(payouts.Records) != 3 {
		t.Fatal(payouts)
	}
	for i, want := range []struct {
		op, id, trace string
		amount        int64
		at            time.Time
	}{
		{"op-a", "payout-a", "trace-a", 505, earlier}, {"op-b", "payout-b", "trace-b", -200, cutoff}, {"op-c", "payout-c", "trace-c", 0, cutoff},
	} {
		p := payouts.Records[i]
		if p.Operation != want.op || p.Payout != want.id || p.Trace != want.trace || p.Amount != want.amount ||
			p.Merchant != "mimi" || p.Currency != "USD" || !p.CreatedAt.Equal(want.at) {
			t.Fatal(p, want)
		}
	}
	var bank timingInventory[bankTimingRecord]
	baseBank := call(t, "GET", bankURL+query, serviceKey, nil, http.StatusOK, &bank)
	if !bank.Complete || bank.Merchant != "mimi" || bank.Currency != "USD" || !bank.AsOf.Equal(cutoff) || len(bank.Records) != 2 {
		t.Fatal(bank)
	}
	for i, want := range []struct {
		trace  string
		amount int64
	}{{"trace-d", 535}, {"trace-z", -600}} {
		b := bank.Records[i]
		if b.Trace != want.trace || b.Amount != want.amount || b.Merchant != "mimi" || b.Currency != "USD" || !b.PostedAt.Equal(cutoff) {
			t.Fatal(b, want)
		}
	}
	var bLater timingInventory[bankTimingRecord]
	call(t, "GET", bankURL+withCutoff(later), serviceKey, nil, http.StatusOK, &bLater)
	if len(bLater.Records) != 3 || bLater.Records[2].Trace != "trace-a" || !bLater.Records[2].PostedAt.Equal(later) {
		t.Fatal(bLater)
	}
	for _, in := range []struct {
		origin string
		first  []byte
	}{{processorURL, base}, {bankURL, baseBank}} {
		var empty timingInventory[json.RawMessage]
		call(t, "GET", in.origin+withCutoff(earlier.Add(-time.Hour)), serviceKey, nil, http.StatusOK, &empty)
		if !empty.Complete || empty.Records == nil || len(empty.Records) != 0 {
			t.Fatal(empty)
		}
		if again := call(t, "GET", in.origin+withCutoff(cutoff), serviceKey, nil, http.StatusOK, nil); !bytes.Equal(in.first, again) {
			t.Fatal("source inventory changed when the same cutoff was replayed")
		}
	}
	if got := sourceState(); got != beforeReads {
		t.Fatal("reading the timing inventory rewrote a source row", beforeReads, got)
	}
	count(t, s.aq, "SELECT count(*) FROM sim_payout", 4)
	count(t, s.bk, "SELECT count(*) FROM sim_bank_entry WHERE posted_at IS NULL", 1)
}

func TestSettlementTimingOversizedInventoryFailsWithoutPartialResults(t *testing.T) {
	s := newTestStore(t)
	// Use valid input lengths and entries with distinct original identities.
	// A small source cutoff still succeeds after a larger one fails.
	_, err := s.bk.Exec(t.Context(), `INSERT INTO sim_bank_entry(trace,merchant_id,currency,amount,received_at,posted_at)
		SELECT repeat('x',100)||n::text,'mimi','USD',1,'2026-01-01Z','2026-01-02Z' FROM generate_series(1,24000) n`)
	if err != nil {
		t.Fatal(err)
	}
	path := s.bank.URL + "/bank/settlement-timing?as_of="
	result := map[string]any{}
	call(t, "GET", path+"2026-01-02T00:00:00Z", serviceKey, nil, http.StatusServiceUnavailable, &result)
	if result["error"] == nil || result["records"] != nil || result["complete"] != nil {
		t.Fatal("partial results presented as evidence", result)
	}
	var empty timingInventory[bankTimingRecord]
	call(t, "GET", path+"2026-01-01T00:00:00Z", serviceKey, nil, http.StatusOK, &empty)
	if !empty.Complete || empty.Records == nil || len(empty.Records) != 0 {
		t.Fatal(empty)
	}
}

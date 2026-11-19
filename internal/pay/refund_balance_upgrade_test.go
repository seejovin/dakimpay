package pay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// This pre-balance-schema state comes from the previous release's lost-refusal
// exercise. A $10 capture paid $9.80 to the merchant; an external refund then
// posted $7.00 while two merchant instructions ($8 and $2) remained unresolved.
func TestRefundBalanceUpgradeKeepsLegacyEvidenceAndSiblingReservation(t *testing.T) {
	ctx, db := t.Context(), isolatedPool(t)
	if _, err := db.Exec(ctx, schema); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(ctx, `
	INSERT INTO pay.settlement_requests(request_key,operation_id,batch) VALUES ('first','ds_first','standard'),('later','ds_later','standard');
	INSERT INTO pay.payouts(id,operation_id,bank_trace,currency,net,status,bank_evidence) VALUES
	 ('po_first','ds_first','bank_first','USD',980,'matched','{"trace":"bank_first","merchant_id":"mimi","amount":980,"currency":"USD","status":"posted"}'),
	 ('po_later','ds_later','bank_later','USD',-700,'matched','{"trace":"bank_later","merchant_id":"mimi","amount":-700,"currency":"USD","status":"posted"}');
	INSERT INTO pay.payments(id,merchant_id,request_key,order_id,channel,terminal_id,brand,currency,amount,status,reference,lose_response)
	VALUES ('dp_original','mimi','sale','sale','terminal','MIMI-01','visa','USD',1000,'captured','ch_original',false),
	       ('dp_pending','mimi','other','other','terminal','MIMI-01','visa','USD',1000,'unknown',NULL,false);
	INSERT INTO pay.refunds(id,request_key,payment_id,amount,currency) VALUES
	 ('dr_competing','competing','dp_original',800,'USD'),('dr_sibling','sibling','dp_original',200,'USD');
	INSERT INTO pay.refunds(id,payment_id,amount,currency,status,reference,source_operation,source_sequence,occurred_at,original_payout)
	VALUES('dr_source','dp_original',700,'USD','succeeded','rf_source','sr_later',nextval('pay.adjustment_sequence'),now(),'po_first');
	INSERT INTO pay.lines(id,payout_id,kind,reference,amount) VALUES
	 ('sale','po_first','capture','ch_original',1000),('fee','po_first','fee','ch_original',-20),('refund','po_later','refund','rf_source',-700);
	INSERT INTO pay.events(subject,kind,message) VALUES ('dr_source','provider_refund','Provider verified and original deposit matched.'),
	 ('dr_competing','reserved','Original request reserved.'),('dr_sibling','reserved','Original request reserved.');`)
	if err != nil {
		t.Fatal(err)
	}
	// Compare entire financial rows and their evidence, including timestamps.
	// After authoritative recovery only the disputed command and its event may
	// change; the sibling, source movement, capture and payouts stay identical.
	history := func(excludeRecovered bool) string {
		t.Helper()
		var result string
		err := db.QueryRow(ctx, `SELECT jsonb_build_array(
		 (SELECT jsonb_agg(p ORDER BY id) FROM pay.payments p),
		 (SELECT jsonb_agg(p ORDER BY id) FROM pay.payouts p),
		 (SELECT jsonb_agg(l ORDER BY id) FROM pay.lines l),
		 (SELECT jsonb_agg(r ORDER BY id) FROM pay.refunds r WHERE NOT $1 OR r.id<>'dr_competing'),
		 (SELECT jsonb_agg(e ORDER BY id) FROM pay.events e WHERE NOT $1 OR e.subject<>'dr_competing'))::text`, excludeRecovered).Scan(&result)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	before, unaffected := history(false), history(true)
	var wrong atomic.Bool
	var inquiries atomic.Int32
	wrong.Store(true)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-upgrade" {
			http.Error(w, "unknown local credential", 403)
			return
		}
		fact := refundRequest{OperationID: "dr_competing", MerchantID: "mimi", ChargeReference: "ch_original", Amount: 800, Currency: "USD"}
		switch r.Method + " " + r.URL.Path {
		case "POST /sim/refunds":
			var req refundRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req != fact {
				t.Errorf("recovered a different refund: %+v, %v", req, err)
			}
			http.Error(w, "original refund declined", 409)
		case "GET /sim/refunds/dr_competing":
			inquiries.Add(1)
			if wrong.Load() {
				fact.OperationID = "dr_sibling"
			}
			answer(w, 200, refundFact{refundRequest: fact, Status: "declined", Reference: ""})
		default:
			t.Errorf("unexpected provider request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer source.Close()
	start := func() *Service {
		t.Helper()
		s, err := New(ctx, db, source.URL, source.URL, "local-upgrade")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	type balance struct {
		succeeded, pending, reserved, available int64
		review                                  bool
	}
	check := func(id string, want balance) {
		t.Helper()
		var got balance
		var currency string
		err := db.QueryRow(ctx, `SELECT currency,succeeded,pending_requested,reserved,available,review FROM pay.refund_balances WHERE payment_id=$1`, id).Scan(&currency, &got.succeeded, &got.pending, &got.reserved, &got.available, &got.review)
		if err != nil || currency != "USD" || got != want {
			t.Fatalf("balance %s: %s %+v, want %+v: %v", id, currency, got, want, err)
		}
	}
	marker := func() string {
		t.Helper()
		var value string
		err := db.QueryRow(ctx, `SELECT to_jsonb(m)::text FROM pay.refund_legacy_reviews m WHERE payment_id='dp_original' AND
		(captured_at_detection,succeeded_at_detection,pending_at_detection)=(1000,700,1000) AND (SELECT count(*) FROM pay.refund_legacy_reviews)=1`).Scan(&value)
		if err != nil {
			t.Fatal("original discrepancy missing", err)
		}
		return value
	}
	s := start()
	check("dp_original", balance{700, 1000, 300, 0, true})
	check("dp_pending", balance{})
	discrepancy := marker()
	s = start() // Reapply the production migration while the conflict is still open.
	if before != history(false) || marker() != discrepancy {
		t.Fatal("upgrade altered prior records")
	}
	check("dp_original", balance{700, 1000, 300, 0, true})
	path := "/v1/refunds/dr_competing/recover"
	if code, out := issue(s.Handler(), http.MethodPost, path, "", nil); code != 502 || inquiries.Load() != 1 {
		t.Fatalf("refusal naming the sibling was not rejected after original inquiry: %d %+v", code, out)
	}
	check("dp_original", balance{700, 1000, 300, 0, true})
	wrong.Store(false)
	if code, out := issue(s.Handler(), http.MethodPost, path, "", nil); code != 200 || out["status"] != "declined" {
		t.Fatalf("exact refusal: %d %+v", code, out)
	}
	check("dp_original", balance{700, 200, 200, 100, false})
	if unaffected != history(true) || marker() != discrepancy {
		t.Fatal("recovering one instruction rewrote its sibling, feed or bank history")
	}
	after := history(false)
	s = start()
	if code, out := issue(s.Handler(), http.MethodPost, path, "", nil); code != 200 || out["status"] != "declined" {
		t.Fatalf("refusal replay: %d %+v", code, out)
	}
	if after != history(false) || marker() != discrepancy {
		t.Fatal("restart/replay rewrote the resolved history")
	}
	check("dp_original", balance{700, 200, 200, 100, false})
}

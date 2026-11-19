package simulator

import (
	"bytes"
	"reflect"
	"testing"
)

func TestFrozenPayoutFeeEvidencePreservesOriginalEstimate(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	base := s.acquirer.URL
	evidenceURL := base + "/sim/payouts/fee-check/fee-evidence"
	call(t, "GET", evidenceURL, serviceKey, nil, 404, nil)
	call(t, "GET", evidenceURL, approveKey, nil, 401, nil)
	count(t, s.aq, "SELECT count(*) FROM sim_payout", 0)
	count(t, s.bk, "SELECT count(*) FROM sim_bank_entry", 0)

	req := chargeRequest{Operation: "fee-sale", Merchant: merchant, Order: "basket-A", Channel: "terminal", Amount: 1275, Currency: currency, Brand: "visa", Terminal: "MIMI-01"}
	var current charge
	call(t, "POST", base+"/sim/charges", serviceKey, req, 200, &current)
	call(t, "POST", base+"/sim/charges", serviceKey, req, 200, nil)
	count(t, s.aq, "SELECT count(*) FROM sim_fee_estimate", 1)
	// Both observations exist before settlement. The estimate remains 26
	// cents (25.5 rounded half up); the final processor statement charges 29.
	if _, err := s.aq.Exec(ctx, "UPDATE sim_line SET amount=-29 WHERE kind='fee' AND reference=$1 AND payout_id IS NULL", current.Reference); err != nil {
		t.Fatal(err)
	}
	if _, err := s.aq.Exec(ctx, "UPDATE sim_fee_estimate SET fee=29 WHERE reference=$1", current.Reference); err == nil {
		t.Fatal("recorded fee quote could be rewritten")
	}
	if _, err := s.aq.Exec(ctx, "DELETE FROM sim_fee_estimate WHERE reference=$1", current.Reference); err == nil {
		t.Fatal("recorded fee quote could be erased")
	}

	// A preceding release stored these two movements without a quote.
	// Re-running the startup schema must not manufacture a historical one.
	if _, err := s.aq.Exec(ctx, `INSERT INTO sim_charge(operation_id,reference,merchant_id,order_id,channel,amount,currency,brand,terminal_id,status,captured_at)
		VALUES ('old-payment','old-capture','mimi','old-basket','terminal',500,'USD','amex','MIMI-01','captured',now());
		INSERT INTO sim_line(kind,reference,amount,batch) VALUES ('capture','old-capture',500,'standard'),('fee','old-capture',-10,'standard');`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.aq.Exec(ctx, acquirerSchema); err != nil {
		t.Fatal(err)
	}
	var frozen payout
	call(t, "POST", base+"/sim/payouts", serviceKey, payoutRequest{"fee-check", merchant, currency, "standard"}, 200, &frozen)
	// This subsequent capture is in the next batch, outside the frozen one.
	req.Operation, req.Order = "later-payment", "basket-B"
	call(t, "POST", base+"/sim/charges", serviceKey, req, 200, nil)
	var evidence payoutFeeEvidence
	first := call(t, "GET", evidenceURL, serviceKey, nil, 200, &evidence)
	if evidence.Operation != "fee-check" || evidence.Merchant != merchant || evidence.Currency != currency ||
		evidence.PayoutID != frozen.ID || evidence.Trace != frozen.Trace || evidence.Net != 1736 ||
		len(evidence.Lines) != 4 || !reflect.DeepEqual(evidence.Lines, frozen.Lines) || len(evidence.Estimates) != 1 {
		t.Fatalf("wrong frozen statement membership or amount: %+v", evidence)
	}
	quote := evidence.Estimates[0]
	if quote.PaymentID != current.Operation || quote.Reference != current.Reference || quote.Amount != req.Amount ||
		quote.Currency != currency || quote.Fee != 26 || quote.RateBPS != 200 || quote.Policy != "sim-card-fees-v1" || quote.RecordedAt.IsZero() {
		t.Fatalf("capture-time quote changed: %+v", quote)
	}
	if again := call(t, "GET", evidenceURL, serviceKey, nil, 200, nil); !bytes.Equal(first, again) {
		t.Fatal("report replay changed the evidence")
	}
	count(t, s.aq, "SELECT count(*) FROM sim_fee_estimate", 2)
	count(t, s.aq, "SELECT count(*) FROM sim_line WHERE payout_id IS NULL", 2)
	count(t, s.aq, "SELECT count(*) FROM sim_payout", 1)
	count(t, s.bk, "SELECT count(*) FROM sim_bank_entry WHERE posted_at IS NULL", 1)
}

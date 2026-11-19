package simulator

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestHostedExpirySerializesApprovalInquiryAndReplay(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	base := s.acquirer.URL
	req := chargeRequest{Operation: "unpaid-original", Merchant: merchant, Order: "expired-basket", Channel: "online", Amount: 580, Currency: currency, Brand: "visa"}
	var opened charge
	call(t, "POST", base+"/sim/charges", serviceKey, req, 200, &opened)
	if _, err := time.Parse(time.RFC3339Nano, opened.CheckoutExpiresAt); err != nil {
		t.Fatal("missing hosted deadline", err)
	}
	count(t, s.aq, "select count(*) from sim_charge where checkout_expires_at between created_at+interval '14 minutes' and created_at+interval '16 minutes'", 1)
	// Shorten just this synthetic checkout's deadline. A lost expiry event
	// must roll back the provider's decision, keeping it available for retry.
	if _, err := s.aq.Exec(ctx, `update sim_charge set checkout_expires_at=clock_timestamp();
		alter table sim_callback_outbox add constraint synthetic_outage check(kind<>'payment.expired')`); err != nil {
		t.Fatal(err)
	}
	if err := expireDueCheckouts(ctx, s.aq); err == nil {
		t.Fatal("failed callback write left a successful checkout decision")
	}
	count(t, s.aq, "select count(*) from sim_charge where status='awaiting_customer'", 1)
	count(t, s.aq, "select count(*) from sim_line", 0)
	if _, err := s.aq.Exec(ctx, "alter table sim_callback_outbox drop constraint synthetic_outage"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	checkDecision := func(method, path, key string, expected int) {
		status, raw, err := request(method, base+path+req.Operation, key, nil)
		if err != nil || status != expected {
			t.Errorf("%s %s: %d, %v", method, path, status, err)
			return
		}
		var decision charge
		if err = json.Unmarshal(raw, &decision); err != nil || decision.Reference != opened.Reference || decision.Status != "expired" || decision.CheckoutURL != "" {
			t.Errorf("unexpected closed checkout: %+v, %v", decision, err)
		}
	}
	for range 4 {
		wg.Go(func() { checkDecision("GET", "/sim/charges/", serviceKey, 200) })
		wg.Go(func() { checkDecision("POST", "/sim/approve/", approveKey, 409) })
		wg.Go(func() {
			if err := expireDueCheckouts(ctx, s.aq); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for _, method := range []string{"GET", "POST"} {
		page := string(call(t, method, opened.CheckoutURL, "", nil, 410, nil))
		if !strings.Contains(page, "Checkout expired") || strings.Contains(page, "<form") {
			t.Fatal("closed checkout offered approval")
		}
	}
	var replay charge
	call(t, "POST", base+"/sim/charges", serviceKey, req, 200, &replay)
	if replay.Status != "expired" || replay.Reference != opened.Reference {
		t.Fatal("create replay issued a fresh session", replay)
	}
	count(t, s.aq, "select count(*) from sim_callback_outbox where kind='payment.expired'", 1)
	count(t, s.aq, "select count(*) from sim_callback_outbox where kind='payment.captured'", 0)
	count(t, s.aq, "select count(*) from sim_charge where captured_at is not null", 0)
	count(t, s.aq, "select count(*) from sim_line", 0)
}

func TestApprovalChecksDeadlineAtDecisionTime(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	req := chargeRequest{Operation: "waiting-at-deadline", Merchant: merchant, Order: "nearly-expired", Channel: "online", Amount: 400, Currency: currency, Brand: "amex"}
	var open charge
	call(t, "POST", s.acquirer.URL+"/sim/charges", serviceKey, req, 200, &open)
	a := acquirer{pool: s.aq, publicURL: s.acquirer.URL}
	err := write(ctx, s.aq, func(tx pgx.Tx) error {
		// Model the clock when an already-open transaction finally wins the
		// lock. PostgreSQL's now() remains frozen before this deadline.
		var afterStart bool
		if err := tx.QueryRow(ctx, `update sim_charge set checkout_expires_at=clock_timestamp()
			where operation_id=$1 returning checkout_expires_at>transaction_timestamp()`, open.Operation).Scan(&afterStart); err != nil {
			return err
		}
		next, err := a.decideCheckout(ctx, tx, open, true)
		if err == nil && (!afterStart || next.Status != "expired" || next.Reference != open.Reference) {
			return fmt.Errorf("old transaction crossed the deadline: %v, %+v", afterStart, next)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	count(t, s.aq, "select count(*) from sim_line", 0)
	count(t, s.aq, "select count(*) from sim_callback_outbox where kind='payment.expired'", 1)
}

func TestHostedExpiryPreservesHistoricalCaptureAndPendingCheckout(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	req := chargeRequest{Operation: "pre-upgrade-open", Merchant: merchant, Order: "open-order", Channel: "online", Amount: 1230, Currency: currency, Brand: "visa"}
	var open, paid charge
	call(t, "POST", s.acquirer.URL+"/sim/charges", serviceKey, req, 200, &open)
	req.Operation, req.Order = "pre-upgrade-paid", "paid-order"
	call(t, "POST", s.acquirer.URL+"/sim/charges", serviceKey, req, 200, nil)
	call(t, "POST", s.acquirer.URL+"/sim/approve/"+req.Operation, approveKey, nil, 200, &paid)
	// Recreate the old schema around actual persisted events and money. Old
	// pending sessions had no deadline and could already be several days old.
	if _, err := s.aq.Exec(ctx, `alter table sim_charge drop column checkout_expires_at,
		drop constraint sim_charge_status_check, add constraint sim_charge_status_check check(status in ('awaiting_customer','captured'));
		update sim_charge set created_at=now()-interval '2 days' where status='awaiting_customer';
		alter table sim_callback_outbox drop constraint sim_callback_event_kind,
		add constraint sim_callback_event_kind check(kind in ('payment.awaiting_customer','payment.captured','refund.succeeded','refund.observed'))`); err != nil {
		t.Fatal(err)
	}
	restart := func() string {
		t.Helper()
		h, err := AcquirerWithKeys(ctx, s.aq, s.acquirer.URL, s.bank.URL, serviceKey, instructKey, approveKey)
		if err != nil {
			t.Fatal(err)
		}
		restarted := httptest.NewServer(h)
		t.Cleanup(restarted.Close)
		return restarted.URL
	}
	first := restart()
	var firstOpen, oldCapture charge
	call(t, "GET", first+"/sim/charges/"+open.Operation, serviceKey, nil, 200, &firstOpen)
	call(t, "GET", first+"/sim/charges/"+paid.Operation, serviceKey, nil, 200, &oldCapture)
	if firstOpen.Reference != open.Reference || firstOpen.CheckoutURL != open.CheckoutURL || firstOpen.Status != "awaiting_customer" || firstOpen.CheckoutExpiresAt == "" {
		t.Fatal("upgrade lost outstanding checkout", firstOpen)
	}
	paid.CheckoutExpiresAt = "" // No deadline was recorded against an old capture.
	if oldCapture != paid {
		t.Fatal("upgrade altered historical capture", oldCapture, paid)
	}
	second := restart()
	var secondOpen charge
	call(t, "GET", second+"/sim/charges/"+open.Operation, serviceKey, nil, 200, &secondOpen)
	if secondOpen != firstOpen {
		t.Fatal("restart extended unpaid checkout", firstOpen, secondOpen)
	}
	var captured charge
	call(t, "POST", second+"/sim/approve/"+open.Operation, approveKey, nil, 200, &captured)
	if captured.Reference != firstOpen.Reference || captured.Status != "captured" {
		t.Fatal("provider lost original reference", captured)
	}
	// Shorten the window to just after the successful capture. The original
	// reference must still be paid when it is first inspected after the deadline.
	if _, err := s.aq.Exec(ctx, "update sim_charge set checkout_expires_at=captured_at+interval '1 microsecond' where operation_id=$1", open.Operation); err != nil {
		t.Fatal(err)
	}
	if err := expireDueCheckouts(ctx, s.aq); err != nil {
		t.Fatal(err)
	}
	call(t, "GET", second+"/sim/charges/"+open.Operation, serviceKey, nil, 200, &captured)
	call(t, "POST", second+"/sim/approve/"+open.Operation, approveKey, nil, 200, nil)
	if captured.Status != "captured" || captured.Reference != open.Reference {
		t.Fatal("capture lost at deadline", captured)
	}
	count(t, s.aq, "select count(*) from sim_line where kind='capture'", 2)
	count(t, s.aq, "select count(*) from sim_callback_outbox where kind='payment.captured'", 2)
	count(t, s.aq, "select count(*) from sim_callback_outbox where kind='payment.expired'", 0)
}

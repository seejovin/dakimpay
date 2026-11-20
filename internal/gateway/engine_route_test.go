package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
)

type routeTestRail struct {
	ledger      map[string]string
	facts       map[string]railcontract.Fact
	brand       string
	submissions []railcontract.Command
	reversals   []railcontract.Fact
}

func (r *routeTestRail) Identity(ctx context.Context, c railcontract.Command) (string, error) {
	got := r.ledger[c.ConnectorID]
	if got == "" {
		return "", errors.New("connector unavailable")
	}
	if pin := railcontract.ExpectedProcessorIdentity(ctx); pin != "" && pin != got {
		return "", railcontract.ErrProcessorIdentityMismatch
	}
	return got, nil
}
func (r *routeTestRail) Submit(ctx context.Context, c railcontract.Command) error {
	if _, err := r.Identity(ctx, c); err != nil {
		return err
	}
	r.submissions = append(r.submissions, c)
	f := railcontract.Fact{Command: c, State: "captured", Reference: "native-001", CardBrand: r.brand}
	if c.Kind == "refund" {
		f.State = "refunded"
		f.Reference = "native-return-001"
	}
	r.facts[c.ID] = f
	return errors.New("lost reply after observed movement")
}
func (r *routeTestRail) Lookup(ctx context.Context, c railcontract.Command) (railcontract.Fact, bool, error) {
	if _, err := r.Identity(ctx, c); err != nil {
		return railcontract.Fact{}, false, err
	}
	f, ok := r.facts[c.ID]
	return f, ok, nil
}
func (r *routeTestRail) InspectRelease(ctx context.Context, c railcontract.Command) (railcontract.ReleaseInventory, error) {
	f, found, err := r.Lookup(ctx, c)
	if !found || err != nil {
		return railcontract.ReleaseInventory{}, errors.New("original unavailable")
	}
	return railcontract.ReleaseInventory{Fact: f, Refunds: r.reversals, DisputeStatus: "none", Complete: true, ObservedAt: time.Now().UTC()}, nil
}

func newRouteTestRail() *routeTestRail {
	return &routeTestRail{ledger: map[string]string{"first": strings.Repeat("a", 64), "second": strings.Repeat("b", 64)}, facts: map[string]railcontract.Fact{}, brand: "visa", reversals: []railcontract.Fact{}}
}
func setTestRoute(c railcontract.Command, provider string) railcontract.Command {
	c.ConnectorID = provider
	c.RouteID = "route-" + provider
	c.ProviderAccount = "same-acquirer-local-mid"
	if c.PaymentMethod == "" {
		c.PaymentMethod = "card"
	}
	return c
}
func readRouteOp(t *testing.T, s *Service, p Principal, id string) engineOperation {
	t.Helper()
	var got engineOperation
	err := s.Store.View(t.Context(), func(tx gwstore.Tx) error { var err error; got, err = engineGet(tx, p.Merchant, id); return err })
	if err != nil {
		t.Fatal(err)
	}
	return got
}
func readRouteRelease(t *testing.T, s *Service, p Principal, id string) engineReleaseDecision {
	t.Helper()
	r := httptest.NewRequest("GET", "/v2/payment-intents/"+id+"/release-evidence", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	var d engineReleaseDecision
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &d) != nil {
		t.Fatalf("release %d %s", w.Code, w.Body.String())
	}
	return d
}
func TestMerchantRouteStaysPinnedAcrossRestartAndRefund(t *testing.T) {
	ctx := t.Context()
	r := newRouteTestRail()
	p := engineTestPrincipal()
	s := New(engineTestStore(t), r)
	in := testIntent()
	in.PaymentMethod = "visa"
	selections := 0
	s.SelectRoute = func(c railcontract.Command) (railcontract.Command, error) {
		selections++
		return setTestRoute(c, "first"), nil
	}
	one, _, err := s.createIntent(ctx, p, "req", in)
	if err != nil || selections != 1 {
		t.Fatalf("first admission: %+v, %d, %v", one, selections, err)
	}
	// The gateway restarts with another preference before the original dispatch.
	s = New(s.Store, r)
	s.CurrentProgram = func(string) (Principal, bool) { return p, true }
	secondPreference := 0
	s.SelectRoute = func(c railcontract.Command) (railcontract.Command, error) {
		secondPreference++
		return setTestRoute(c, "second"), nil
	}
	old, replay, err := s.createIntent(ctx, p, "req", in)
	if err != nil || !replay || old.Command != one.Command || secondPreference != 0 {
		t.Fatal("replay consulted routing policy", old, err)
	}
	changed := in
	changed.PaymentMethod = "mastercard"
	if _, _, err = s.createIntent(ctx, p, "req", changed); err == nil || err.Error() != "idempotency_conflict" {
		t.Fatalf("reused terms: %v", err)
	}
	if err = s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readRouteOp(t, s, p, one.Command.ID); got.Status != "captured" || got.CardBrand != "visa" || got.Reference != "native-001" {
		t.Fatalf("first acquired fact: %+v", got)
	}
	if got := readRouteRelease(t, s, p, one.Command.ID); !got.Eligible {
		t.Fatalf("clean original held: %+v", got)
	}
	refund, _, err := s.createRefund(ctx, p, "r1", one.Command.ID, engineRefundInput{BusinessID: "refund-1", Amount: 100})
	if err != nil || !matchesProcessorRoute(refund.Command, one.Command) || refund.Ledger != one.Ledger || secondPreference != 0 {
		t.Fatal("refund rerouted", refund, err)
	}
	in.Order = "another-order"
	two, _, err := s.createIntent(ctx, p, "req-2", in)
	if err != nil || two.Command.RouteID != "route-second" || secondPreference != 1 || two.Ledger == one.Ledger {
		t.Fatal("second route not enrolled", two, err)
	}
	if err = s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readRouteOp(t, s, p, two.Command.ID); got.Status != "captured" || got.Reference != "native-001" {
		t.Fatalf("equal provider-native references collided: %+v", got)
	}
	if got := readRouteOp(t, s, p, refund.Command.ID); got.Status != "refunded" || got.Command.ConnectorID != "first" {
		t.Fatal("refund reached alternative route", got)
	}
	if len(r.submissions) != 3 {
		t.Fatal("unexpected submissions", r.submissions)
	}
	wrong := r.facts[refund.Command.ID]
	wrong.ConnectorID = "second"
	r.reversals = []railcontract.Fact{wrong}
	if d := readRouteRelease(t, s, p, one.Command.ID); d.Complete || d.Eligible || d.Reason != "processor_evidence_conflict" {
		t.Fatalf("foreign refund evidence accepted: %+v", d)
	}
}

func TestBrandMismatchBooksObservedMoneyAndBlocksGoods(t *testing.T) {
	ctx := t.Context()
	r := newRouteTestRail()
	r.brand = "amex"
	p := engineTestPrincipal()
	s := New(engineTestStore(t), r)
	s.SelectRoute = func(c railcontract.Command) (railcontract.Command, error) { return setTestRoute(c, "first"), nil }
	in := testIntent()
	in.PaymentMethod = "visa"
	op, _, err := s.createIntent(ctx, p, "sale", in)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := readRouteOp(t, s, p, op.Command.ID)
	if got.Status != "captured" || got.CardBrand != "amex" || got.Reason != "card_brand_mismatch" {
		t.Fatalf("missing economic or brand exception: %+v", got)
	}
	if released := readRouteRelease(t, s, p, op.Command.ID); released.Eligible || released.Reason != "card_brand_mismatch" {
		t.Fatalf("off-scheme sale released: %+v", released)
	}
	err = s.Store.View(ctx, func(tx gwstore.Tx) error {
		row, ok, err := readPosting(tx, bookPrefix(p.Merchant, p.Account, p.Currency), FinancialRoute{RouteID: op.Command.RouteID, ProviderAccount: op.Command.ProviderAccount}, "capture", got.Reference)
		if err == nil && (!ok || row.Legs[0].Debit != in.Amount || row.CardBrand != "amex") {
			t.Fatalf("observed money missing from books: %+v", row)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRoutesRejectLedgerAliasesAndGrantRewrites(t *testing.T) {
	ctx := t.Context()
	r := newRouteTestRail()
	r.ledger["second"] = r.ledger["first"]
	p := engineTestPrincipal()
	s := New(engineTestStore(t), r)
	in := testIntent()
	in.Channel = "hosted"
	in.Terminal = ""
	s.SelectRoute = func(c railcontract.Command) (railcontract.Command, error) { return setTestRoute(c, "first"), nil }
	if _, _, err := s.createIntent(ctx, p, "order-a", in); err != nil {
		t.Fatal(err)
	}
	in.Order = "another"
	s.SelectRoute = func(c railcontract.Command) (railcontract.Command, error) { return setTestRoute(c, "second"), nil }
	if _, _, err := s.createIntent(ctx, p, "order-b", in); err == nil || err.Error() != "processor_account_already_enrolled" {
		t.Fatal("duplicate connector alias linked", err)
	}
	s.SelectRoute = func(c railcontract.Command) (railcontract.Command, error) {
		c = setTestRoute(c, "first")
		c.Amount++
		return c, nil
	}
	if _, _, err := s.createIntent(ctx, p, "order-c", in); err == nil || err.Error() != "invalid_processor_route" {
		t.Fatal("selector rewrote the sale", err)
	}
}
func TestCheckoutFragmentIsScopedToCanonicalStripeTestSessions(t *testing.T) {
	f := railcontract.Fact{Command: railcontract.Command{Kind: "charge", Channel: "hosted"}, State: "pending"}
	for _, x := range []struct {
		url string
		ok  bool
	}{
		{"https://checkout.stripe.com/c/pay/cs_test_123#buyer-capability", true},
		{"https://checkout.stripe.com:999/c/pay/cs_test_123#buyer-capability", false},
		{"https://example.com/c/pay/cs_test_123#buyer-capability", false},
		{"https://checkout.stripe.com/c/pay/cs_live_123#buyer-capability", false},
		{"https://checkout.stripe.com/c/pay/cs_test_123/other#buyer-capability", false},
		{"https://checkout.stripe.com/c/pay/cs_test_123?a=b#buyer-capability", false},
	} {
		f.CheckoutURL = x.url
		if engineValidFact(f) != x.ok {
			t.Fatalf("url %q, want %t", x.url, x.ok)
		}
	}
}

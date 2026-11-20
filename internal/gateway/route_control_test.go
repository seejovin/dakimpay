package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"cloud.google.com/go/spanner"
	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
)

type routeIncidentRail struct {
	*routeTestRail
	onFirstIdentity func()
}

func (r *routeIncidentRail) Identity(ctx context.Context, c railcontract.Command) (string, error) {
	if r.onFirstIdentity != nil {
		action := r.onFirstIdentity
		r.onFirstIdentity = nil
		action()
	}
	return r.routeTestRail.Identity(ctx, c)
}

func TestDurableRouteControls(t *testing.T) {
	t.Run("postgres", func(t *testing.T) { testDurableRouteControls(t, engineTestStore(t)) })
	t.Run("spanner", func(t *testing.T) {
		name := os.Getenv("DAKIM_TEST_SPANNER_DATABASE")
		if name == "" || os.Getenv("SPANNER_EMULATOR_HOST") == "" {
			t.Skip("requires disposable Spanner emulator")
		}
		client, err := spanner.NewClient(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		store, err := gwstore.NewSpanner(t.Context(), client)
		if err != nil {
			t.Fatal(err)
		}
		testDurableRouteControls(t, store)
	})
}

func testDurableRouteControls(t *testing.T, store gwstore.Store) {
	ctx := t.Context()
	p := engineTestPrincipal()
	unique, err := engineID("ctrl_")
	if err != nil {
		t.Fatal(err)
	}
	p.Merchant = unique
	maker := p
	maker.Role = FinanceRole
	maker.Subject = "finance"
	checker := p
	checker.Role = ReviewerRole
	checker.Subject = "reviewer"
	other := maker
	other.Merchant = unique + "-outside"
	r := &routeIncidentRail{routeTestRail: newRouteTestRail()}
	configure := func() *Service {
		s := New(store, r)
		s.RouteCandidates = func(c railcontract.Command) []railcontract.Command {
			a, b := setTestRoute(c, "first"), setTestRoute(c, "second")
			a.ProviderAccount, b.ProviderAccount = unique, unique
			if c.PaymentMethod == "alipay" {
				return []railcontract.Command{a}
			}
			return []railcontract.Command{a, b}
		}
		s.RouteExists = func(m, a, id string) bool {
			return m == p.Merchant && a == p.Account && (id == "route-first" || id == "route-second")
		}
		return s
	}
	s := configure()
	call := func(actor Principal, method, route, action, key string, data any, status int) map[string]any {
		t.Helper()
		body, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, "/v2/program/routes/"+route+"/sales"+action, bytes.NewReader(body))
		req.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req.WithContext(WithPrincipal(ctx, actor)))
		if w.Code != status {
			t.Fatalf("%s %s%s code %d want %d: %s", method, route, action, w.Code, status, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	terms := testIntent()
	terms.Channel = "hosted"
	terms.Terminal = ""
	terms.PaymentMethod = "visa"
	first, _, err := s.createIntent(ctx, p, "one", terms)
	if err != nil {
		t.Fatal(err)
	}
	terms.Order = "recovered"
	recoverable, _, err := s.createIntent(ctx, p, "two", terms)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Submit(railcontract.WithProcessorIdentity(ctx, recoverable.Ledger), recoverable.Command)
	if len(r.submissions) != 1 {
		t.Fatal("fixture did not accept original")
	}
	call(other, "POST", "route-first", "/freeze", "incident", map[string]any{"reason": "outside tenant"}, 404)
	call(p, "POST", "route-first", "/freeze", "incident", map[string]any{"reason": "merchant"}, 403)
	incident := map[string]any{"reason": "broken processor link"}
	// Drain while the original A identity observation is returning, before
	// the new payment is durably recorded. The replacement B must prove its
	// own ledger in a fresh inquiry and admission transaction.
	r.onFirstIdentity = func() { call(maker, "POST", "route-first", "/freeze", "incident", incident, 200) }
	terms.Order = "new-after-drain"
	third, _, err := s.createIntent(ctx, p, "three", terms)
	if err != nil || third.Command.ConnectorID != "second" || third.Ledger != r.ledger["second"] {
		t.Fatalf("failed clean fallback: %+v %v", third, err)
	}
	s = configure()
	status := call(p, "GET", "route-first", "", "", nil, 200)
	if status["suspended"] != true {
		t.Fatalf("failed to restore: %v", status)
	}
	if receipt := call(maker, "POST", "route-first", "/freeze", "incident", incident, 200); receipt["sequence"] != float64(1) {
		t.Fatal("failed freeze replay", receipt)
	}
	methods, err := s.AvailableMethods(ctx, p, "hosted", 500, []string{"visa", "alipay"})
	if err != nil || len(methods) != 1 || methods[0] != "visa" {
		t.Fatal("discovery ignored route drain", methods, err)
	}
	if err = s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readRouteOp(t, s, p, first.Command.ID); got.EgressCommitted || got.Reason != "original_processor_route_suspended" || got.Command.ConnectorID != "first" {
		t.Fatalf("previously admitted original failed drain: %+v", got)
	}
	if got := readRouteOp(t, s, p, recoverable.Command.ID); got.Status != "captured" {
		t.Fatalf("existing sale not recovered: %+v", got)
	}
	if got := readRouteOp(t, s, p, third.Command.ID); got.Status != "captured" {
		t.Fatalf("fallback sale not captured: %+v", got)
	}
	if len(r.submissions) != 2 {
		t.Fatal("unexpected egress", r.submissions)
	}
	refund, _, err := s.createRefund(ctx, p, "refund", recoverable.Command.ID, engineRefundInput{BusinessID: "credit", Amount: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readRouteOp(t, s, p, refund.Command.ID); got.Status != "refunded" || got.Command.RouteID != "route-first" {
		t.Fatalf("original-acquirer refund blocked: %+v", got)
	}
	call(maker, "POST", "route-second", "/freeze", "incident", incident, 200)
	terms.Order = "all-drained"
	if _, _, err = s.createIntent(ctx, p, "four", terms); err == nil || err.Error() != "payment_routes_suspended" {
		t.Fatal("all drained: expected blocked admission", err)
	}
	proposal := call(maker, "POST", "route-first", "/resume-proposals", "repair", map[string]any{"reason": "verified partner recovery", "suspension_version": 1}, 200)
	binding := map[string]any{"proposal_id": proposal["proposal_id"], "proposal_digest": proposal["proposal_digest"], "suspension_version": 1}
	self := checker
	self.Subject = maker.Subject
	call(self, "POST", "route-first", "/resume-approvals", "sign", binding, 403)
	call(checker, "POST", "route-second", "/resume-approvals", "sign", binding, 404)
	call(checker, "POST", "route-first", "/resume-approvals", "sign", binding, 200)
	audit := call(maker, "GET", "route-first", "/audit", "", nil, 200)
	if len(audit["events"].([]any)) != 3 {
		t.Fatal("unexpected route control history", audit)
	}
	four, _, err := s.createIntent(ctx, p, "four", terms)
	if err != nil || four.Command.RouteID != "route-first" {
		t.Fatalf("restored preference: %+v %v", four, err)
	}
}

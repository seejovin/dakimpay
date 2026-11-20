package aggregation

import (
	"context"
	"errors"
	"slices"
	"testing"

	"dakimpay/internal/railcontract"
)

type observedProcessor struct {
	submits, lookups, inspections int
	last                          railcontract.Command
	lookup                        railcontract.Fact
	found                         bool
	submitErr                     error
	identity                      string
	inventory                     railcontract.ReleaseInventory
}

func (p *observedProcessor) Submit(_ context.Context, c railcontract.Command) error {
	p.submits++
	p.last = c
	return p.submitErr
}
func (p *observedProcessor) Lookup(_ context.Context, c railcontract.Command) (railcontract.Fact, bool, error) {
	p.lookups++
	p.last = c
	return p.lookup, p.found, nil
}
func (p *observedProcessor) Identity(_ context.Context, c railcontract.Command) (string, error) {
	p.last = c
	return p.identity, nil
}
func (p *observedProcessor) InspectRelease(_ context.Context, c railcontract.Command) (railcontract.ReleaseInventory, error) {
	p.inspections++
	p.last = c
	return p.inventory, nil
}
func card() Route {
	return Route{ID: "cards", Merchant: "tenant", Account: "program", Connector: "processor-a", ProviderAccount: "MID-12", Currency: "USD",
		Methods: []string{"card", "visa", "mastercard", "amex"}, Channels: []string{"hosted", "terminal"}, MinAmount: 10, MaxAmount: 10000, Priority: 1, Enabled: true}
}
func charge() railcontract.Command {
	return railcontract.Command{ID: "pi_1", Merchant: "tenant", Account: "program", Kind: "charge", Order: "order1", Channel: "hosted", Amount: 150, Currency: "USD"}
}
func build(t *testing.T, routes []Route, a, b *observedProcessor, prior railcontract.Processor) *Router {
	t.Helper()
	r, err := New(routes, map[string]railcontract.Processor{"processor-a": a, "processor-b": b}, prior)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCatalogSelectionAndDiscovery(t *testing.T) {
	primary := card()
	backup := card()
	backup.ID, backup.Connector, backup.ProviderAccount, backup.Priority = "backup", "processor-b", "other-MID", 2
	backup.MinAmount, backup.MaxAmount = 1, 100000
	wallet := card()
	wallet.ID, wallet.Connector, wallet.ProviderAccount, wallet.Methods, wallet.Channels = "wallet", "processor-b", "wallet-MID", []string{"alipay"}, []string{"hosted"}
	otherTenant := wallet
	otherTenant.Merchant, otherTenant.ProviderAccount, otherTenant.Methods = "foreign", "foreign-MID", []string{"wechat_pay"}
	r := build(t, []Route{backup, primary, wallet, otherTenant}, &observedProcessor{}, &observedProcessor{}, nil)
	// The caller may reuse configuration slices after building a snapshot.
	primary.Methods[0], primary.Channels[0] = "bad", "bad"
	for _, tc := range []struct {
		amount       int64
		method, want string
	}{
		{1, "", "backup"}, {9, "visa", "backup"}, {10, "card", "cards"}, {10000, "amex", "cards"},
		{10001, "mastercard", "backup"}, {100001, "card", ""}, {10, "alipay", "wallet"}, {9, "alipay", ""},
		{10, "wechat_pay", ""}, {10, "Visa", ""}, {0, "", ""},
	} {
		req := charge()
		req.Amount, req.PaymentMethod = tc.amount, tc.method
		got, err := r.Select(req)
		if tc.want == "" {
			if !errors.Is(err, ErrNoEligibleRoute) {
				t.Fatalf("eligible %v? %v, %v", tc, got, err)
			}
			continue
		}
		if err != nil || got.RouteID != tc.want || got.PaymentMethod == "" || got.ProviderAccount == "" || got.ConnectorID == "" || !r.CanDispatch(got) {
			t.Fatalf("selection for %v: %#v, %v", tc, got, err)
		}
	}
	for _, change := range []func(*railcontract.Command){
		func(c *railcontract.Command) { c.Currency = "EUR" },
		func(c *railcontract.Command) { c.Account = "unknown" },
		func(c *railcontract.Command) { c.Merchant = "unknown" },
		func(c *railcontract.Command) { c.RouteID = "wallet" },
		func(c *railcontract.Command) { c.PaymentMethod, c.Channel = "alipay", "terminal" },
	} {
		c := charge()
		change(&c)
		if _, err := r.Select(c); !errors.Is(err, ErrNoEligibleRoute) {
			t.Errorf("unauthorized charge accepted: %#v", c)
		}
	}
	if methods := r.Methods("tenant", "program", "USD", "hosted", 0); !slices.Equal(methods, []string{"card", "visa", "mastercard", "amex", "alipay"}) {
		t.Fatalf("discovery leaked or omitted a method: %v", methods)
	}
	if methods := r.Methods("tenant", "program", "USD", "terminal", 10); slices.Contains(methods, "alipay") || slices.Contains(methods, "wechat_pay") {
		t.Fatalf("terminal advertised a wallet: %v", methods)
	}
	if methods := r.Methods("foreign", "program", "USD", "hosted", 10); !slices.Equal(methods, []string{"wechat_pay"}) {
		t.Fatalf("cross-tenant discovery: %v", methods)
	}
	if got := r.Methods("tenant", "program", "USD", "hosted", 100001); len(got) != 0 {
		t.Fatalf("excessive amount: %v", got)
	}
}

func TestOriginalOnlyAfterSuspensionTimeoutAndRemap(t *testing.T) {
	ctx := context.Background()
	a, b := &observedProcessor{identity: "ledger-a"}, &observedProcessor{identity: "ledger-b"}
	first, second := card(), card()
	second.ID, second.Connector, second.ProviderAccount, second.Priority = "backup", "processor-b", "MID-99", 2
	r := build(t, []Route{first, second}, a, b, nil)
	original, err := r.Select(charge())
	if err != nil {
		t.Fatal(err)
	}
	// A timeout can follow remote acceptance. Router must return it without another POST.
	a.submitErr = context.DeadlineExceeded
	if err = r.Submit(ctx, original); !errors.Is(err, context.DeadlineExceeded) || a.submits != 1 || b.submits != 0 {
		t.Fatalf("timeout semantics: %v, submits %d/%d", err, a.submits, b.submits)
	}
	first.Enabled = false
	r = build(t, []Route{second, first}, a, b, nil)
	next, err := r.Select(charge())
	if err != nil || next.RouteID != second.ID || next.ProviderAccount != second.ProviderAccount {
		t.Fatalf("new sale: %#v, %v", next, err)
	}
	if r.CanDispatch(original) || !errors.Is(r.Submit(ctx, original), ErrPinnedRoute) {
		t.Fatal("disabled original rerouted or sent")
	}
	a.found, a.lookup = true, railcontract.Fact{Command: original, State: "captured", Reference: "ref"}
	f, found, err := r.Lookup(ctx, original)
	if err != nil || !found || a.lookups != 1 || b.lookups != 0 || f != a.lookup {
		t.Fatal("original could not be recovered")
	}
	if id, err := r.Identity(ctx, original); err != nil || id != a.identity {
		t.Fatal("ledger identity changed")
	}
	a.inventory = railcontract.ReleaseInventory{Fact: a.lookup, Complete: true}
	if _, err := r.InspectRelease(ctx, original); err != nil || a.inspections != 1 || b.inspections != 0 {
		t.Fatal("release left original processor")
	}
	refund := original
	refund.ID, refund.Kind, refund.Parent, refund.Amount = "rf_1", "refund", original.ID, 1
	a.submitErr = nil
	if !r.CanDispatch(refund) || r.Submit(ctx, refund) != nil || a.last != refund || b.submits != 0 {
		t.Fatal("small original refund failed after suspension")
	}
	first.Connector = second.Connector
	r = build(t, []Route{first, second}, a, b, nil)
	if _, _, err := r.Lookup(ctx, original); !errors.Is(err, ErrPinnedRoute) || b.lookups != 0 {
		t.Fatal("changed source accepted")
	}
}

func TestMismatchedEvidenceAndPartialPinsFailClosed(t *testing.T) {
	ctx := context.Background()
	p, old := &observedProcessor{}, &observedProcessor{identity: "old"}
	r := build(t, []Route{card()}, p, &observedProcessor{}, old)
	original, err := r.Select(charge())
	if err != nil {
		t.Fatal(err)
	}
	wrong := original
	wrong.RouteID = "foreign"
	p.found, p.lookup = true, railcontract.Fact{Command: wrong, State: "captured", Reference: "overlap"}
	if _, found, err := r.Lookup(ctx, original); found || !errors.Is(err, railcontract.ErrEvidenceConflict) {
		t.Fatal("foreign processor fact accepted")
	}
	p.inventory = railcontract.ReleaseInventory{Fact: railcontract.Fact{Command: original}, Refunds: []railcontract.Fact{{Command: wrong}}}
	if _, err := r.InspectRelease(ctx, original); !errors.Is(err, railcontract.ErrEvidenceConflict) {
		t.Fatal("foreign refund inventory accepted")
	}
	// Missing a pin component must never select the old default processor.
	bad := original
	bad.RouteID = ""
	if _, _, err := r.Lookup(ctx, bad); !errors.Is(err, ErrPinnedRoute) || old.lookups != 0 {
		t.Fatal("partial pin fell back")
	}
	legacy := charge()
	if id, err := r.Identity(ctx, legacy); err != nil || id != "old" {
		t.Fatal("legacy inquiry unavailable")
	}
	if r.CanDispatch(legacy) || !errors.Is(r.Submit(ctx, legacy), ErrPinnedRoute) {
		t.Fatal("new unbound egress permitted")
	}
	legacy.Kind, legacy.Parent, legacy.ID = "refund", "pi_1", "rf_2"
	if err := r.Submit(ctx, legacy); err != nil || old.submits != 1 {
		t.Fatal("legacy original return unavailable")
	}
}

func TestConfigurationRejectsAmbiguity(t *testing.T) {
	cs := map[string]railcontract.Processor{"processor-a": &observedProcessor{}}
	base := card()
	for _, change := range []func(*Route){
		func(r *Route) { r.ID = "alias" }, // equal priority and intersecting capability
		func(r *Route) { r.Priority = 9 }, // duplicate durable route ID
		func(r *Route) { r.ID = "b"; r.MinAmount = 0 },
		func(r *Route) { r.ID = "b"; r.Connector = "no-client" },
		func(r *Route) { r.ID = "b"; r.Methods = []string{"alipay"} }, // virtual terminal not qualified for wallet
		func(r *Route) { r.ID = "b"; r.Methods = []string{"visa", "visa"} },
		func(r *Route) { r.ID = "b"; r.Currency = "usd" },
	} {
		b := card()
		change(&b)
		if _, err := New([]Route{base, b}, cs, nil); err == nil {
			t.Fatalf("invalid catalog accepted: %#v", b)
		}
	}
	foreign := base
	foreign.Merchant = "other-merchant"
	if _, err := New([]Route{base, foreign}, cs, nil); err == nil {
		t.Fatal("enrolled another merchant on the same acquirer MID")
	}
	var typedNil *observedProcessor
	if _, err := New([]Route{base}, map[string]railcontract.Processor{"processor-a": typedNil}, nil); err == nil {
		t.Fatal("accepted typed-nil client")
	}
}

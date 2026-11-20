package partnerstripe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"dakimpay/internal/railcontract"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	sync.Mutex
	t                       *testing.T
	account                 string
	paid, lose, wrongAmount bool
	inventoryMode           string
	posts                   int
	sessions                map[string]url.Values
	refunds                 map[string]url.Values
}

func (f *fixture) RoundTrip(r *http.Request) (*http.Response, error) {
	f.Lock()
	defer f.Unlock()
	if r.URL.Scheme != "https" || r.URL.Host != "api.stripe.com" || r.Header.Get("Stripe-Version") != "2025-06-30.basil" {
		f.t.Fatal("wrong API origin or version")
	}
	u, _, ok := r.BasicAuth()
	if !ok || !strings.HasPrefix(u, "sk_test_") {
		f.t.Fatal("wrong auth")
	}
	var data any
	path := r.URL.Path
	switch {
	case path == "/v1/account":
		data = map[string]any{"object": "account", "id": f.account, "country": "US"}
	case r.Method == "POST":
		f.posts++
		if !strings.HasPrefix(r.Header.Get("Idempotency-Key"), "dakim-") || r.GetBody != nil {
			f.t.Fatal("unsafe create request")
		}
		body, _ := io.ReadAll(r.Body)
		p, _ := url.ParseQuery(string(body))
		op := p.Get("metadata[dakim_operation]")
		if path == "/v1/checkout/sessions" {
			id := "cs_test_" + op
			f.sessions[id] = p
			data = map[string]any{"object": "checkout.session", "id": id, "livemode": false}
			if p.Get("payment_method_types[0]") == "wechat_pay" && p.Get("payment_method_options[wechat_pay][client]") != "web" {
				f.t.Fatal("missing WeChat web client")
			}
			if p.Get("adaptive_pricing[enabled]") != "false" || p.Get("payment_intent_data[metadata][dakim_terms]") != p.Get("metadata[dakim_terms]") {
				f.t.Fatal("missing immutable purchase terms")
			}
		} else if path == "/v1/refunds" {
			id := "re_" + op
			f.refunds[id] = p
			data = map[string]any{"object": "refund", "id": id}
		} else {
			f.t.Fatalf("unexpected POST path %s", path)
		}
		if f.lose {
			f.lose = false
			return nil, io.ErrUnexpectedEOF
		}
	case strings.HasPrefix(path, "/v1/checkout/sessions/"):
		id := strings.TrimPrefix(path, "/v1/checkout/sessions/")
		p := f.sessions[id]
		if p == nil || r.URL.Query().Get("expand[]") != "payment_intent.latest_charge" {
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
		}
		cur := p.Get("line_items[0][price_data][currency]")
		amount := int64(2300)
		if f.wrongAmount {
			amount++
		}
		op := p.Get("metadata[dakim_operation]")
		m := p.Get("payment_method_types[0]")
		meta := map[string]string{"dakim_ledger": p.Get("metadata[dakim_ledger]"), "dakim_terms": p.Get("metadata[dakim_terms]"), "dakim_operation": op}
		v := map[string]any{"object": "checkout.session", "id": id, "livemode": false, "mode": "payment", "url": "https://checkout.stripe.com/c/pay/" + id + "#opaque", "client_reference_id": op, "amount_total": amount, "currency": cur, "payment_method_types": []string{m}, "metadata": meta, "status": "open", "payment_status": "unpaid"}
		if f.paid {
			v["status"] = "complete"
			v["payment_status"] = "paid"
			details := map[string]any{"type": m}
			if m == "card" {
				details["card"] = map[string]string{"brand": "visa", "network": "visa"}
			}
			ch := map[string]any{"object": "charge", "id": "ch_" + op, "payment_intent": "pi_" + op, "livemode": false, "status": "succeeded", "paid": true, "captured": true, "disputed": false, "refunded": false, "amount_refunded": 0, "currency": cur, "amount": amount, "amount_captured": amount, "payment_method_details": details}
			v["payment_intent"] = map[string]any{"object": "payment_intent", "id": "pi_" + op, "livemode": false, "status": "succeeded", "amount": amount, "amount_received": amount, "currency": cur, "metadata": meta, "latest_charge": ch}
		}
		data = v
	case (path == "/v1/refunds" || path == "/v1/disputes") && r.Method == "GET":
		if !strings.HasPrefix(r.URL.Query().Get("charge"), "ch_") || r.URL.Query().Get("limit") != "1" {
			f.t.Fatal("unscoped collection")
		}
		page := map[string]any{"object": "list", "url": path, "has_more": false, "data": []any{}}
		if f.inventoryMode == path {
			page["data"] = []any{map[string]string{"charge": r.URL.Query().Get("charge")}}
		}
		if f.inventoryMode == "malformed" {
			delete(page, "data")
		}
		if f.inventoryMode == "incomplete" {
			page["has_more"] = true
		}
		if f.inventoryMode == "drift" && path == "/v1/disputes" {
			f.wrongAmount = true
		}
		if f.inventoryMode == "denied" {
			return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
		}
		data = page
	case strings.HasPrefix(path, "/v1/refunds/"):
		id := strings.TrimPrefix(path, "/v1/refunds/")
		p := f.refunds[id]
		if p == nil {
			f.t.Fatal("missing refund")
		}
		data = map[string]any{"object": "refund", "id": id, "status": "succeeded", "currency": "usd", "charge": p.Get("charge"), "amount": 300, "metadata": map[string]string{"dakim_ledger": p.Get("metadata[dakim_ledger]"), "dakim_terms": p.Get("metadata[dakim_terms]"), "dakim_operation": p.Get("metadata[dakim_operation]")}}
	default:
		f.t.Fatalf("unexpected HTTP URL %s", r.URL.Path)
	}
	b, _ := json.Marshal(data)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b))), Header: http.Header{}}, nil
}
func testConfig() Config {
	return Config{TestKey: "sk_test_localfixture000000000", AccountID: "acct_local123", Merchant: "mimi", GatewayAccount: "mimi-us", ConnectorID: "stripe-test", ReturnURL: "https://merchant.example.test/return", AllowedRoutes: map[string][]string{"route": {"card", "alipay", "wechat_pay"}}, Capabilities: map[string][]string{"card": {"USD"}, "alipay": {"USD"}, "wechat_pay": {"USD"}}}
}
func TestSandboxProgramRestrictions(t *testing.T) {
	c := testConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.TestKey = "sk_live_localfixture000000000"
	if c.Validate() == nil {
		t.Fatal("accepted live key")
	}
	if eligible("MY", "wechat_pay", "MYR") || eligible("US", "alipay", "MYR") || !eligible("MY", "alipay", "MYR") {
		t.Fatal("wallet regional guard")
	}
	c = testConfig()
	c.AllowedRoutes["route"] = []string{"visa"}
	if c.Validate() == nil {
		t.Fatal("advertised a specific card brand at selection")
	}
}
func TestDurableSandboxOriginals(t *testing.T) {
	dsn := os.Getenv("DAKIM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a disposable PostgreSQL server")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	dbname := fmt.Sprintf("dk_stripe_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+dbname); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP DATABASE "+dbname)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = dbname
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := testConfig()
	wire := &fixture{t: t, account: c.AccountID, sessions: map[string]url.Values{}, refunds: map[string]url.Values{}}
	client := &http.Client{Transport: wire}
	a, err := newAdapter(ctx, db, c, client)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"card", "alipay", "wechat_pay"} {
		op := railcontract.Command{ID: "sale" + m, Merchant: c.Merchant, Account: c.GatewayAccount, PaymentMethod: m, RouteID: "route", ConnectorID: c.ConnectorID, ProviderAccount: c.AccountID, Kind: "charge", Order: "order" + m, Outlet: "store", Channel: "hosted", Amount: 2300, Currency: "USD"}
		ledger, err := a.Identity(ctx, op)
		if err != nil {
			t.Fatal(err)
		}
		pinned := railcontract.WithProcessorIdentity(ctx, ledger)
		if _, ok, err := a.Lookup(pinned, op); err != nil || ok {
			t.Fatalf("unexpected existing %v %v", ok, err)
		}
		if _, _, err := a.Lookup(railcontract.WithProcessorIdentity(ctx, "changed"), op); !errors.Is(err, railcontract.ErrProcessorIdentityMismatch) {
			t.Fatalf("identity mismatch %v", err)
		}
		wire.paid = false
		wire.lose = m == "card"
		before := wire.posts
		err = a.Submit(pinned, op)
		if m == "card" {
			if err == nil {
				t.Fatal("missing committed-response loss")
			}
			if _, _, err = a.Lookup(pinned, op); !errors.Is(err, ErrUnknown) {
				t.Fatalf("lost response classified as absence: %v", err)
			}
			a, err = newAdapter(ctx, db, c, client)
			if err != nil {
				t.Fatal(err)
			}
			if err = a.Submit(pinned, op); !errors.Is(err, ErrUnknown) {
				t.Fatal("resubmitted ambiguous original")
			}
			if _, err = a.Recover(ctx, op.ID, "cs_test_missing"); err == nil {
				t.Fatal("attached unverified recovery candidate")
			}
			if _, err = a.Recover(ctx, op.ID, "cs_test_"+op.ID); err != nil {
				t.Fatal(err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		if err = a.Submit(pinned, op); err != nil || wire.posts != before+1 {
			t.Fatalf("duplicate POST %v", err)
		}
		fact, found, err := a.Lookup(pinned, op)
		if !found || err != nil || fact.State != "pending" || !strings.HasPrefix(fact.CheckoutURL, "https://checkout.stripe.com/") {
			t.Fatalf("missing pending URL %v %v", fact, err)
		}
		wire.paid = true
		ev, err := a.Read(ctx, op.ID)
		if err != nil || ev.Fact.Reference != "ch_"+op.ID || ev.Fact.State != "captured" || (m == "card" && (ev.CardNetwork != "visa" || ev.Fact.CardBrand != "visa")) {
			t.Fatalf("wrong verified capture %#v %v", ev, err)
		}
		wire.wrongAmount = true
		if _, _, err = a.Lookup(pinned, op); !errors.Is(err, ErrConflict) {
			t.Fatal("accepted inconsistent external observation")
		}
		wire.wrongAmount = false
		inventory, err := a.InspectRelease(pinned, op)
		if err != nil || !inventory.Complete || inventory.DisputeStatus != "none" || inventory.Refunds == nil || inventory.Fact.Reference != ev.Fact.Reference {
			t.Fatalf("missing empty provider inventories %#v %v", inventory, err)
		}
		for _, mode := range []string{"/v1/refunds", "/v1/disputes", "malformed", "incomplete", "drift", "denied"} {
			wire.inventoryMode = mode
			inv, err := a.InspectRelease(pinned, op)
			if inv.Complete || ((mode == "malformed" || mode == "drift" || mode == "denied") && err == nil) {
				t.Fatalf("accepted incomplete provider evidence during %s %#v %v", mode, inv, err)
			}
			wire.inventoryMode = ""
			wire.wrongAmount = false
		}
		rf := op
		rf.Kind = "refund"
		rf.Parent = op.ID
		rf.ID = "refund" + m
		rf.Amount = 300
		if err = a.Submit(pinned, rf); err != nil {
			t.Fatal(err)
		}
		f, ok, err := a.Lookup(pinned, rf)
		if !ok || err != nil || f.State != "refunded" || f.Reference != "re_"+rf.ID {
			t.Fatalf("wrong refund %#v %v", f, err)
		}
		if wire.refunds[f.Reference].Get("charge") != ev.Fact.Reference {
			t.Fatal("refund targets wrong original")
		}
	}
	t.Run("gateway_http_route_to_original_refund", func(t *testing.T) { gatewayJourney(t, db, a, wire) })
	wire.account = "acct_swapped"
	if _, err := newAdapter(ctx, db, c, client); !errors.Is(err, railcontract.ErrProcessorIdentityMismatch) {
		t.Fatalf("account rebind %v", err)
	}
}

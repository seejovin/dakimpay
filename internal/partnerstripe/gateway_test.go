package partnerstripe

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dakimpay/internal/aggregation"
	"dakimpay/internal/gateway"
	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Run an actual gateway HTTP handler and durable worker with the provider
// adapter behind the same route multiplexer used by the gateway binary.
func gatewayJourney(t *testing.T, db *pgxpool.Pool, a *Adapter, wire *fixture) {
	ctx := context.Background()
	c := a.cfg
	if err := gwstore.InstallPostgres(ctx, db); err != nil {
		t.Fatal(err)
	}
	store, err := gwstore.NewPostgres(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	r, err := aggregation.New([]aggregation.Route{{ID: "route", Merchant: c.Merchant, Account: c.GatewayAccount, Connector: c.ConnectorID, ProviderAccount: c.AccountID, Currency: "USD", Methods: []string{"card", "alipay", "wechat_pay"}, Channels: []string{"hosted"}, MinAmount: 1, MaxAmount: 100000, Priority: 1, Enabled: true}}, map[string]railcontract.Processor{c.ConnectorID: a}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := gateway.New(store, r)
	s.SelectRoute = r.Select
	s.RouteCandidates = r.RouteCandidates
	s.RouteExists = r.RouteExists
	s.CanDispatch = r.CanDispatch
	p := gateway.Principal{Merchant: c.Merchant, Account: c.GatewayAccount, Role: gateway.MerchantRole, Subject: "checkout", Beneficiary: "mimi-bank", Currency: "USD", Outlets: map[string]bool{"store": true}, MaxAmount: 100000}
	s.CurrentProgram = func(m string) (gateway.Principal, bool) { return p, m == p.Merchant }
	base := time.Now().UTC()
	// Only the test worker advances its processing schedule. Processor and
	// release-evidence timestamps remain the actual observation time.
	s.Now = func() time.Time { return base }
	h := s.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(gateway.WithPrincipal(r.Context(), p)))
	}))
	defer srv.Close()
	api := func(method, path, key string, input map[string]any, want int) map[string]any {
		b, _ := json.Marshal(input)
		req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var result map[string]any
		if err = json.NewDecoder(res.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("%s %s got %d %v", method, path, res.StatusCode, result)
		}
		return result
	}
	wire.paid = false
	initial := wire.posts
	body := map[string]any{"order_id": "checkout-900", "allocation_id": "primary", "outlet_id": "store", "channel": "hosted", "payment_method": "card", "currency": "USD", "amount_minor": 2300}
	created := api("POST", "/v2/payment-intents", "checkout-900", body, 201)
	id := created["id"].(string)
	uri := "/v2/payment-intents/" + id
	if replay := api("POST", "/v2/payment-intents", "checkout-900", body, 200); replay["id"] != id {
		t.Fatal("changed original")
	}
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := api("GET", uri, "", nil, 200); got["status"] != "pending" || got["route_id"] != "route" || got["provider_account"] != c.AccountID {
		t.Fatalf("unexpected checkout %#v", got)
	}
	wire.paid = true
	base = base.Add(4 * time.Second)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := api("GET", uri, "", nil, 200); got["status"] != "captured" || got["card_brand"] != "visa" {
		t.Fatalf("unexpected capture %#v", got)
	}
	if got := api("GET", uri+"/release-evidence", "", nil, 200); got["release_eligible"] != true {
		t.Fatalf("unreversed sale unexpectedly held %#v", got)
	}
	wire.inventoryMode = "/v1/disputes"
	if got := api("GET", uri+"/release-evidence", "", nil, 200); got["release_eligible"] != false {
		t.Fatalf("external dispute released goods %#v", got)
	}
	wire.inventoryMode = ""
	originalRefund := api("POST", uri+"/refunds", "return-900", map[string]any{"merchant_refund_id": "return-900", "amount_minor": 300}, 201)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := api("GET", "/v2/refunds/"+originalRefund["id"].(string), "", nil, 200); got["status"] != "refunded" || got["route_id"] != "route" {
		t.Fatalf("unexpected refund %#v", got)
	}
	if wire.posts != initial+2 {
		t.Fatal("unexpected number of financial POSTs")
	}
}

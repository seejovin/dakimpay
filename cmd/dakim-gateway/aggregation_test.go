package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"context"
	"dakimpay/internal/aggregation"
	"dakimpay/internal/gateway"
	"dakimpay/internal/gwstore"
)

func oneCatalog() gatewayConfig {
	cfg := perimeterConfig()
	cfg.RailOrigin, cfg.RailToken = "", ""
	cfg.Connectors = []connectorConfig{{ID: "east", Origin: "http://127.0.0.1:8881", Token: strings.Repeat("A", 45)}, {ID: "west", Origin: "http://127.0.0.1:8882", Token: strings.Repeat("B", 45)}}
	cfg.Routes = []aggregation.Route{{ID: "acquiring", Merchant: "one", Account: "acct-one", Connector: "east", ProviderAccount: "east-one", Currency: "USD", Methods: []string{"card", "visa", "mastercard", "amex"}, Channels: []string{"hosted", "terminal"}, MinAmount: 1, MaxAmount: 10000, Priority: 1, Enabled: true}, {ID: "wallet", Merchant: "one", Account: "acct-one", Connector: "west", ProviderAccount: "wallet-one", Currency: "USD", Methods: []string{"alipay"}, Channels: []string{"hosted"}, MinAmount: 250, MaxAmount: 5000, Priority: 1, Enabled: true}}
	cfg.Credentials = append(cfg.Credentials, credentialConfig{Merchant: "one", Role: gateway.StatementRole, Subject: "east-importer", RouteID: "acquiring", ProviderAccount: "east-one", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Repeat("x", 40))))})
	return cfg
}
func TestAggregatorEnforcesConnectorAndStatementAuthority(t *testing.T) {
	for name, mutate := range map[string]func(*gatewayConfig){
		"duplicate credential":  func(c *gatewayConfig) { c.Connectors[1].Token = c.Connectors[0].Token },
		"duplicate destination": func(c *gatewayConfig) { c.Connectors[1].Origin = c.Connectors[0].Origin },
		"merchant with processor token": func(c *gatewayConfig) {
			c.Credentials[0].SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(c.Connectors[0].Token)))
		},
		"foreign native account":   func(c *gatewayConfig) { c.Credentials[len(c.Credentials)-1].ProviderAccount = "wallet-one" },
		"non-import role":          func(c *gatewayConfig) { c.Credentials[len(c.Credentials)-1].Role = gateway.MerchantRole },
		"foreign merchant program": func(c *gatewayConfig) { c.Routes[0].Account = "acct-two" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := oneCatalog()
			mutate(&cfg)
			if _, err := cfg.principals(); err == nil {
				t.Fatal("invalid catalog accepted")
			}
		})
	}
}
func TestAuthenticatedMethodDiscovery(t *testing.T) {
	cfg := oneCatalog()
	grants, err := cfg.principals()
	if err != nil {
		t.Fatal(err)
	}
	_, router, done, err := cfg.openConnectors(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	core := &gateway.Service{Store: readOnlyFixture{}, RouteCandidates: router.RouteCandidates, SelectRoute: router.Select}
	h := registerHTTP(core, grants, router, cfg)
	for _, v := range []struct {
		merchant string
		role     gateway.Role
		query    string
		want     int
		wallet   bool
	}{
		{"one", gateway.MerchantRole, "channel=hosted&amount_minor=500", 200, true},
		{"one", gateway.MerchantRole, "channel=hosted&amount_minor=249", 200, false},
		{"one", gateway.MerchantRole, "channel=terminal&amount_minor=500", 200, false},
		{"two", gateway.MerchantRole, "channel=hosted&amount_minor=500", 200, false},
		{"one", gateway.FinanceRole, "channel=hosted", 403, false},
		{"one", gateway.MerchantRole, "channel=hosted&channel=terminal", 400, false},
	} {
		req := httptest.NewRequest(http.MethodGet, "/v2/payment-methods?"+v.query, nil)
		req.Header.Set("Authorization", "Bearer "+programToken(v.merchant, v.role))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != v.want || strings.Contains(w.Body.String(), "\"alipay\"") != v.wallet {
			t.Fatalf("discovery %s %s %d %s", v.merchant, v.query, w.Code, w.Body.String())
		}
	}
}

// Empty readable control state models a newly configured program.
type readOnlyFixture struct{ gwstore.Store }

func (readOnlyFixture) View(_ context.Context, f func(gwstore.Tx) error) error { return f(emptyRows{}) }

type emptyRows struct{ gwstore.Tx }

func (emptyRows) Get(string) ([]byte, bool, error) { return nil, false, nil }

func TestPersistentNativeMerchantAndStoreOwnership(t *testing.T) {
	dsn := os.Getenv("DAKIM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PostgreSQL fixture required")
	}
	ctx := t.Context()
	pool, err := localPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = gwstore.InstallPostgres(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store, err := gwstore.NewPostgres(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	scope := fmt.Sprintf("fixture-%d", time.Now().UnixNano())
	owner := nativeOwner{"merchant-one", "program-one", "conn-one", "r1"}
	retry := func(who nativeOwner) error {
		return claimPartner(ctx, store, "test", []string{scope, scope + "-identity"}, who)
	}
	if err = retry(owner); err != nil {
		t.Fatal(err)
	}
	if err = retry(owner); err != nil {
		t.Fatal(err)
	}
	other := owner
	other.Connector = "conn-two"
	if retry(other) == nil {
		t.Fatal("upstream authority reassigned to a different connector")
	}
	other = owner
	other.Merchant = "merchant-two"
	if retry(other) == nil {
		t.Fatal("upstream authority reassigned to a different merchant")
	}
}

func TestLazyPartnerDoesNotDialUntilSelectedAndCoalesces(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	finish := make(chan struct{})
	lazy := &lazyPartner{open: func(ctx context.Context) (fullProcessor, error) {
		calls.Add(1)
		close(started)
		<-finish
		return nil, errors.New("partner unavailable")
	}}
	if calls.Load() != 0 {
		t.Fatal("contacted an unselected connector")
	}
	var group sync.WaitGroup
	for i := 0; i < 4; i++ {
		group.Add(1)
		go func() { defer group.Done(); _, _ = lazy.client(t.Context()) }()
	}
	<-started
	close(finish)
	group.Wait()
	if calls.Load() != 1 {
		t.Fatal("unavailable connector initiated duplicate account checks")
	}
}

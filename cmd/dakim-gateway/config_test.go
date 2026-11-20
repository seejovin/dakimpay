package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dakimpay/internal/gateway"
)

const (
	apiOrigin  = "http://127.0.0.1:8470"
	railOrigin = "http://127.0.0.1:8471"
)

func programToken(merchant string, role gateway.Role) string {
	return "test-fixture-only-credential-" + merchant + "-" + string(role)
}

func perimeterConfig() gatewayConfig {
	c := gatewayConfig{Origin: apiOrigin, RailOrigin: railOrigin, RailToken: strings.Repeat("r", 40)}
	for _, m := range []string{"one", "two"} {
		outlet, terminal := "shop-"+m, "Lane-"+m
		c.Programs = append(c.Programs, programConfig{Merchant: m, Account: "acct-" + m, Beneficiary: "bank-" + m, Currency: "USD", MaxAmount: 10000, Outlets: []string{outlet}, Terminals: map[string]string{terminal: outlet}})
		for _, r := range []gateway.Role{gateway.MerchantRole, gateway.StatementRole, gateway.BankRole, gateway.FinanceRole, gateway.ReviewerRole} {
			c.Credentials = append(c.Credentials, credentialConfig{Merchant: m, Role: r, Subject: m + "-" + string(r), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(programToken(m, r))))})
		}
	}
	return c
}

func TestPrivateConfigInput(t *testing.T) {
	raw, err := json.Marshal(perimeterConfig())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var good gatewayConfig
	if err := readConfig(path, &good); err != nil || good.Programs[0].Terminals["Lane-one"] != "shop-one" {
		t.Fatalf("private typed configuration: %v", err)
	}
	link := path + "-link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if readConfig(link, &gatewayConfig{}) == nil {
		t.Fatal("followed a symlink to key material")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if readConfig(path, &gatewayConfig{}) == nil {
		t.Fatal("accepted publicly readable enrollment")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"trailing": string(raw) + ` {}`, "unknown": `{"environment":"live"}`, "case alias": `{"ORIGIN":"x"}`,
		"duplicate": `{"origin":"x","origin":"y"}`, "nested duplicate": `{"programs":[{"terminals":{"Lane1":"east","Lane1":"west"}}]}`,
		"nested alias": `{"credentials":[{"ROLE":"finance"}]}`, "null": `{"origin":null}`, "large": strings.Repeat(" ", 1<<20) + "{}",
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if readConfig(path, &gatewayConfig{}) == nil {
				t.Fatalf("accepted %s input", name)
			}
		})
	}
}

func TestVirtualEndpoints(t *testing.T) {
	if _, err := loopbackOrigin(apiOrigin); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"https://127.0.0.1:8000", "http://example.test:8000", "http://localhost:8000", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://127.0.0.1:8470?", "http://127.0.0.1:8470#", "http://127.0.0.1:8470/", "http://127.0.0.1:08470", "http://user@127.0.0.1:8470"} {
		if _, err := loopbackOrigin(origin); err == nil {
			t.Errorf("accepted origin %q", origin)
		}
	}
	for _, endpoint := range []string{"", "spanner.googleapis.com:443", "192.0.2.4:8080", "127.0.0.1:65536"} {
		if emulatorEndpoint(endpoint) {
			t.Errorf("accepted emulator %q", endpoint)
		}
	}
	for _, host := range []string{"127.0.0.1", "::1", t.TempDir()} {
		pool, err := localPostgres(context.Background(), "host="+host+" sslmode=disable pool_min_conns=0")
		if err != nil {
			t.Errorf("local PG %s: %v", host, err)
		} else {
			pool.Close()
		}
	}
	for _, host := range []string{"192.0.2.4", "db.example.invalid", "127.0.0.1,192.0.2.4"} {
		pool, err := localPostgres(context.Background(), "host="+host+" sslmode=disable")
		if err == nil {
			pool.Close()
			t.Errorf("accepted remote PG %q", host)
		}
	}
}

func TestCredentialScopeAndRoleValidation(t *testing.T) {
	for name, mutate := range map[string]func(*gatewayConfig){
		"remote origin":     func(c *gatewayConfig) { c.RailOrigin = "http://example.test:8000" },
		"shared origin":     func(c *gatewayConfig) { c.RailOrigin = c.Origin },
		"short service key": func(c *gatewayConfig) { c.RailToken = "short" },
		"shared account":    func(c *gatewayConfig) { c.Programs[1].Account = c.Programs[0].Account },
		"unassigned outlet": func(c *gatewayConfig) { c.Programs[0].Terminals["stray"] = "foreign" },
		"missing merchant":  func(c *gatewayConfig) { c.Credentials[0].Merchant = "foreign" },
		"unknown role":      func(c *gatewayConfig) { c.Credentials[0].Role = "superuser" },
		"shared grant":      func(c *gatewayConfig) { c.Credentials[0].SHA256 = c.Credentials[1].SHA256 },
		"rail grant": func(c *gatewayConfig) {
			c.Credentials[0].SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(c.RailToken)))
		},
		"foreign webhook": func(c *gatewayConfig) {
			c.Subscriptions = []gateway.Subscription{{ID: "sub", Merchant: "one", Account: "acct-two", URL: "http://127.0.0.1:9000/hook", KeyID: "k1", Secret: strings.Repeat("k", 40)}}
		},
		"reused signing key": func(c *gatewayConfig) {
			c.Subscriptions = []gateway.Subscription{{ID: "sub", Merchant: "one", Account: "acct-one", URL: "http://127.0.0.1:9000/hook", KeyID: "k1", Secret: programToken("two", gateway.FinanceRole)}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := perimeterConfig()
			mutate(&c)
			if _, err := c.principals(); err == nil {
				t.Fatal("ambiguous or unauthorized enrollment accepted")
			}
		})
	}
}

func TestHTTPPerimeterAndBearerScope(t *testing.T) {
	c := perimeterConfig()
	principals, err := c.principals()
	if err != nil {
		t.Fatal(err)
	}
	api := gatewayHTTP(gateway.New(nil, nil), principals)
	healthy := true
	h := guard("127.0.0.1:8470", apiOrigin, "gateway", func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded readiness probe")
		}
		if !healthy {
			return errors.New("unavailable")
		}
		return nil
	}, api)
	post := func(method, path, host string, auth, origins []string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, apiOrigin+path, nil)
		req.Host = host
		for _, v := range auth {
			req.Header.Add("Authorization", v)
		}
		for _, v := range origins {
			req.Header.Add("Origin", v)
		}
		// No caller-supplied account, role or merchant may override a grant.
		req.Header.Set("X-Merchant-Id", "two")
		req.Header.Set("X-Role", "reviewer")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	host := "127.0.0.1:8470"
	merchant := "Bearer " + programToken("one", gateway.MerchantRole)
	for _, tc := range []struct {
		name, method, path, host string
		auth, origins            []string
		want                     int
	}{
		{"health", "GET", "/health", host, nil, nil, 200}, {"host", "GET", "/health", "bad.test", nil, nil, 421},
		{"cross origin", "GET", "/ui/profile", host, []string{merchant}, []string{"http://bad.test"}, 403},
		{"ambiguous origin", "GET", "/ui/profile", host, []string{merchant}, []string{apiOrigin, "http://bad.test"}, 403},
		{"unauthenticated", "GET", "/ui/profile?merchant_id=two", host, nil, nil, 401},
		{"rail bearer", "GET", "/ui/profile", host, []string{"Bearer " + c.RailToken}, nil, 401},
		{"ambiguous bearer", "GET", "/ui/profile", host, []string{merchant, "Bearer " + programToken("two", gateway.BankRole)}, nil, 401},
		{"bank cannot charge", "POST", "/v2/payment-intents", host, []string{"Bearer " + programToken("one", gateway.BankRole)}, nil, 403},
		{"merchant cannot sign statement", "POST", "/v1/settlement-statements", host, []string{merchant}, nil, 403},
		{"finance cannot approve", "POST", "/v1/payouts/x/approve-close", host, []string{"Bearer " + programToken("one", gateway.FinanceRole)}, nil, 403},
		{"reviewer cannot prepare", "POST", "/v1/payouts/x/prepare-close", host, []string{"Bearer " + programToken("one", gateway.ReviewerRole)}, nil, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := post(tc.method, tc.path, tc.host, tc.auth, tc.origins)
			if w.Code != tc.want {
				t.Fatalf("status %d, expected %d", w.Code, tc.want)
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Frame-Options") != "DENY" {
				t.Fatal("incomplete perimeter headers")
			}
		})
	}
	w := post("GET", "/ui/profile?merchant_id=two", host, []string{merchant}, []string{apiOrigin})
	var profile struct {
		Merchant  string            `json:"merchant"`
		Terminals map[string]string `json:"terminals"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &profile) != nil || profile.Merchant != "one" || len(profile.Terminals) != 1 || profile.Terminals["Lane-one"] != "shop-one" || bytes.Contains(w.Body.Bytes(), []byte("shop-two")) {
		t.Fatalf("profile crossed a grant: %s", w.Body.String())
	}
	healthy = false
	if w := post("GET", "/health", host, nil, nil); w.Code != 503 {
		t.Fatalf("unhealthy dependency reported %d", w.Code)
	}
	public := post("GET", "/", host, nil, nil)
	if public.Code != 200 || !strings.Contains(public.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("public terminal assets bypassed browser isolation policy")
	}
}

func TestPerimeterBodyLimit(t *testing.T) {
	h := guard("127.0.0.1:8470", apiOrigin, "gateway", nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		var limit *http.MaxBytesError
		if !errors.As(err, &limit) || limit.Limit != 256<<10 {
			t.Fatal("request did not hit the perimeter cap")
		}
		w.WriteHeader(413)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", apiOrigin+"/v2/payment-intents", strings.NewReader(strings.Repeat("x", 256<<10+1))))
	if w.Code != 413 {
		t.Fatalf("status %d", w.Code)
	}
}

func TestRailCanSeparateDeviceExportAndConnector(t *testing.T) {
	connector, export, device := strings.Repeat("c", 32), strings.Repeat("e", 32), strings.Repeat("d", 32)
	for _, tc := range []struct {
		name, export, device string
		valid                bool
	}{
		{"distinct", export, device, true},
		{"export reused by device", export, export, false},
		{"connector reused for export", connector, device, false},
		{"connector reused by device", export, connector, false},
		{"short reader grant", export, "short", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg railConfig
			wire := fmt.Sprintf(`{"origin":%q,"rail_token":%q,"programs":[{"beneficiary":"bank","merchant":"one","account":"acct","currency":"USD","max_amount":1000,"outlets":["shop"],"statement_token":%q,"terminals":[{"id":"lane","outlet":"shop","token":%q}]}]}`, railOrigin, connector, tc.export, tc.device)
			if err := json.Unmarshal([]byte(wire), &cfg); err != nil {
				t.Fatal(err)
			}
			ps, err := cfg.programs()
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t, err=%v", tc.valid, err)
			}
			if tc.valid && (len(ps) != 1 || len(ps[0].Terminals) != 1 || ps[0].Terminals[0].ID != "lane" || ps[0].Terminals[0].Outlet != "shop") {
				t.Fatal("different reader enrollment was emitted")
			}
		})
	}
}

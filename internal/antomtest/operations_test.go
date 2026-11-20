package antomtest

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"dakimpay/internal/aggregation"
	"dakimpay/internal/gateway"
	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
	"github.com/jackc/pgx/v5/pgxpool"
)

type sandboxFixture struct {
	t                  *testing.T
	mu                 sync.Mutex
	merchant           *rsa.PublicKey
	antom              *rsa.PrivateKey
	clientID           string
	now                time.Time
	chargeID, refundID string
	refundMetadata     string
	payments, refunds  int
	paid               bool
	fault              string
}

func (f *sandboxFixture) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.URL.Scheme != "https" || req.URL.Host != "open-sea-global.alipay.com" || !strings.HasPrefix(req.URL.Path, "/ams/sandbox/api/v1/payments/") || req.URL.RawQuery != "" {
		f.t.Fatal("request left test API")
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	// Rebuild the independent provider's preimage directly from the HTTP wire.
	client, when := req.Header.Get("Client-Id"), req.Header.Get("Request-Time")
	parts := strings.Split(req.Header.Get("Signature"), ",")
	if len(parts) != 3 || parts[0] != "algorithm=RSA256" || parts[1] != "keyVersion=1" || client != f.clientID || when != f.now.Format(time.RFC3339Nano) {
		f.t.Fatal("bad request headers")
	}
	encoded, err := url.QueryUnescape(strings.TrimPrefix(parts[2], "signature="))
	if err != nil {
		f.t.Fatal(err)
	}
	sign, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		f.t.Fatal(err)
	}
	data := sha256.Sum256(append([]byte(req.Method+" "+req.URL.Path+"\n"+client+"."+when+"."), raw...))
	if rsa.VerifyPKCS1v15(f.merchant, crypto.SHA256, data[:], sign) != nil {
		f.t.Fatal("invalid merchant signature")
	}
	var in map[string]any
	if err = json.Unmarshal(raw, &in); err != nil {
		f.t.Fatal(err)
	}
	if !strings.HasSuffix(req.URL.Path, "/refund") && in["merchantAccountId"] != "2188001" {
		f.t.Fatal("no scoped MID")
	}
	ok := map[string]string{"resultCode": "SUCCESS", "resultStatus": "S"}
	out := map[string]any{"result": ok}
	method := strings.TrimPrefix(req.URL.Path, "/ams/sandbox/api/v1/payments/")
	switch method {
	case "pay":
		f.payments++
		if f.payments != 1 {
			f.t.Fatal("second financial POST")
		}
		f.chargeID = in["paymentRequestId"].(string)
		if in["settlementStrategy"].(map[string]any)["settlementCurrency"] != "USD" || in["productCode"] != "CASHIER_PAYMENT" || in["paymentMethod"].(map[string]any)["paymentMethodType"] != "ALIPAY_CN" ||
			in["paymentAmount"].(map[string]any)["value"] != "1234" || in["order"].(map[string]any)["referenceOrderId"] != f.chargeID {
			f.t.Fatal("incorrect charge")
		}
		out["result"] = map[string]string{"resultCode": "PAYMENT_IN_PROCESS", "resultStatus": "U"}
		out["paymentId"], out["paymentRequestId"], out["paymentAmount"], out["normalUrl"] = "pay123", f.chargeID, money{"USD", "1234"}, "https://ac.alipay.com/page/sandbox/stone-infostaging.html?opaque=token"
	case "inquiryPayment":
		out["paymentId"], out["paymentRequestId"], out["paymentAmount"] = "pay123", in["paymentRequestId"], money{"USD", "1234"}
		if f.paid {
			out["paymentStatus"] = "SUCCESS"
		} else {
			out["paymentStatus"] = "PROCESSING"
		}
	case "refund":
		f.refunds++
		f.refundID = in["refundRequestId"].(string)
		f.refundMetadata = in["metadata"].(string)
		if f.refunds != 1 || in["paymentId"] != "pay123" || in["refundAmount"].(map[string]any)["value"] != "800" {
			f.t.Fatal("wrong refund")
		}
		if f.fault == "disconnect" {
			return nil, io.ErrUnexpectedEOF
		}
		out["refundId"], out["refundRequestId"] = "refund124", f.refundID
	case "inquiryRefund":
		if in["refundRequestId"] != f.refundID {
			f.t.Fatal("wrong queried original refund")
		}
		out["metadata"] = f.refundMetadata
		out["refundId"], out["refundRequestId"], out["refundStatus"], out["refundAmount"] = "refund124", f.refundID, "SUCCESS", money{"USD", "800"}
	default:
		f.t.Fatal("unknown API")
	}
	stamp := f.now
	switch f.fault {
	case "stale":
		stamp = stamp.Add(-10 * time.Minute)
	case "earlier":
		stamp = stamp.Add(-time.Minute)
	case "wrong-original":
		out["paymentRequestId"] = "foreign123"
	case "wrong-payment-id":
		out["paymentId"] = "another123"
	case "wrong-amount":
		out["paymentAmount"] = money{"USD", "999"}
	case "foreign-account":
		out["merchantAccountId"] = "different"
	case "wrong-refund":
		out["refundRequestId"] = "anotherRefund"
	case "wrong-binding":
		out["metadata"] = "another"
	case "wrong-parent":
		out["paymentId"] = "foreignPayment"
	case "unknown":
		out["result"] = map[string]string{"resultCode": "ORDER_NOT_EXIST", "resultStatus": "F"}
	case "regression":
		out["paymentStatus"] = "PROCESSING"
	}
	body, err := json.Marshal(out)
	if err != nil {
		f.t.Fatal(err)
	}
	if f.fault == "duplicate" {
		body = append([]byte(`{"paymentStatus":"FAIL",`), body[1:]...)
	}
	peerTime := stamp.Format(time.RFC3339Nano)
	digest := sha256.Sum256(append([]byte("POST "+req.URL.Path+"\n"+f.clientID+"."+peerTime+"."), body...))
	proof, err := rsa.SignPKCS1v15(rand.Reader, f.antom, crypto.SHA256, digest[:])
	if err != nil {
		f.t.Fatal(err)
	}
	if f.fault == "bad-signature" {
		proof[0] ^= 0xFF
	}
	headers := http.Header{}
	headers.Set("Client-Id", f.clientID)
	headers.Set("Response-Time", peerTime)
	headers.Set("Signature", "algorithm=RSA256,keyVersion=2,signature="+url.QueryEscape(base64.StdEncoding.EncodeToString(proof)))
	if f.fault == "duplicate-signature" {
		headers.Add("Signature", headers.Get("Signature"))
	}
	if f.fault == "foreign-client" {
		headers.Set("Client-Id", "SANDBOX_other")
	}
	return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
}

// Namespacing keeps this test replayable on a shared test database without
// removing any existing data. The real connector uses the dedicated DB.
type fixtureStore struct {
	gwstore.Store
	prefix string
}
type fixtureTx struct {
	gwstore.Tx
	prefix string
}

func (t fixtureTx) Get(k string) ([]byte, bool, error) { return t.Tx.Get(t.prefix + k) }
func (t fixtureTx) Put(k string, v []byte) error       { return t.Tx.Put(t.prefix+k, v) }
func (t fixtureTx) Delete(k string) error              { return t.Tx.Delete(t.prefix + k) }
func (t fixtureTx) Scan(prefix, after string, limit int) ([]gwstore.Entry, error) {
	if after != "" {
		after = t.prefix + after
	}
	rows, err := t.Tx.Scan(t.prefix+prefix, after, limit)
	for i := range rows {
		rows[i].Key = strings.TrimPrefix(rows[i].Key, t.prefix)
	}
	return rows, err
}
func (s fixtureStore) View(ctx context.Context, fn func(gwstore.Tx) error) error {
	return s.Store.View(ctx, func(tx gwstore.Tx) error { return fn(fixtureTx{tx, s.prefix}) })
}
func (s fixtureStore) Update(ctx context.Context, fn func(gwstore.Tx) error) error {
	return s.Store.Update(ctx, func(tx gwstore.Tx) error { return fn(fixtureTx{tx, s.prefix}) })
}

func TestSignedSandboxContract(t *testing.T) {
	dsn := os.Getenv("DAKIM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires test PostgreSQL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = gwstore.InstallPostgres(ctx, pool); err != nil {
		t.Fatal(err)
	}
	base, err := gwstore.NewPostgres(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	db := fixtureStore{base, fmt.Sprintf("test/antom/%d/", time.Now().UnixNano())}
	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&peer.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Merchant: "testMerchant", GatewayAccount: "testProgram", ConnectorID: "partner", RouteID: "alipay-apac", Currency: "USD", ClientID: "SANDBOX_TEST001", MerchantAccountID: "2188001", Region: "asia", MerchantKeyVersion: "1", AntomKeyVersion: "2", RedirectURL: "https://merchant.example/return",
		MerchantPrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(pk)})),
		AntomPublicKeyPEM:     string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}))}
	fx := &sandboxFixture{t: t, merchant: &pk.PublicKey, antom: peer, clientID: cfg.ClientID, now: time.Now().UTC()}
	build := func() *Client {
		c, err := New(cfg, db)
		if err != nil {
			t.Fatal(err)
		}
		c.now = func() time.Time { return fx.now }
		c.http.Transport = fx
		return c
	}
	c := build()
	cmd := railcontract.Command{ID: "pi_001", Kind: "charge", Order: "basket-987", Outlet: "o1", Channel: "hosted", Currency: "USD", Amount: 1234, Merchant: "testMerchant",
		Account: "testProgram", PaymentMethod: "alipay", RouteID: "alipay-apac", ConnectorID: "partner", ProviderAccount: cfg.MerchantAccountID}
	id, err := c.Identity(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	ctx = railcontract.WithProcessorIdentity(ctx, id)
	if _, found, err := c.Lookup(ctx, cmd); err != nil || found {
		t.Fatal("fresh namespace not absent", err)
	}
	if err = c.Submit(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	if err = c.Submit(ctx, cmd); err != nil || fx.payments != 1 {
		t.Fatal("repeat POST", err)
	}
	got, found, err := c.Lookup(ctx, cmd)
	if err != nil || !found || got.State != "pending" || got.CheckoutURL == "" {
		t.Fatal("pending checkout", got, err)
	}
	for _, bad := range []string{"stale", "earlier", "wrong-original", "wrong-payment-id", "wrong-amount", "duplicate", "bad-signature", "duplicate-signature", "foreign-account", "foreign-client"} {
		t.Run(bad, func(t *testing.T) {
			fx.fault = bad
			if _, _, err := c.Lookup(ctx, cmd); !errors.Is(err, railcontract.ErrEvidenceConflict) {
				t.Fatalf("invalid evidence deemed sufficient: %v", err)
			}
			fx.fault = ""
		})
	}
	fx.fault = "unknown"
	if _, _, err := c.Lookup(ctx, cmd); err == nil {
		t.Fatal("missing remote record after egress authorized absence")
	}
	fx.fault = ""
	fx.paid = true
	c = build() // same durable connector, new process
	got, found, err = c.Lookup(ctx, cmd)
	if err != nil || !found || got.Reference != "pay123" || got.State != "captured" {
		t.Fatal(got, err)
	}
	fx.fault = "regression"
	if _, _, err := c.Lookup(ctx, cmd); !errors.Is(err, railcontract.ErrEvidenceConflict) {
		t.Fatal("accepted final-state regression")
	}
	fx.fault = ""
	inv, err := c.InspectRelease(ctx, cmd)
	if err != nil || inv.Complete || inv.DisputeStatus != "unknown" {
		t.Fatal("incomplete independent release evidence", inv, err)
	}
	refund := cmd
	refund.Kind, refund.Amount, refund.Parent, refund.ID = "refund", 800, cmd.ID, "rf_002"
	fx.fault = "disconnect"
	if err = c.Submit(ctx, refund); err == nil {
		t.Fatal("expected lost original refund response")
	}
	fx.fault = ""
	c = build()
	if err = c.Submit(ctx, refund); err != nil || fx.refunds != 1 {
		t.Fatal("refund reissued after restart", err)
	}
	for _, bad := range []string{"wrong-refund", "wrong-parent", "wrong-binding"} {
		fx.fault = bad
		if _, _, err := c.Lookup(ctx, refund); !errors.Is(err, railcontract.ErrEvidenceConflict) {
			t.Fatal("invalid refund binding", bad)
		}
	}
	fx.fault = ""
	got, found, err = c.Lookup(ctx, refund)
	if err != nil || !found || got.State != "refunded" || got.Reference != "refund124" {
		t.Fatal("original refund", got, err)
	}
	for _, change := range []func(*railcontract.Command){
		func(cmd *railcontract.Command) { cmd.Merchant = "intruder" },
		func(cmd *railcontract.Command) { cmd.Account = "otherProgram" },
		func(cmd *railcontract.Command) { cmd.RouteID = "differentRoute" },
		func(cmd *railcontract.Command) { cmd.ConnectorID = "differentConnector" },
		func(cmd *railcontract.Command) { cmd.Currency = "EUR" },
	} {
		mismatch := cmd
		change(&mismatch)
		if _, err := c.Identity(ctx, mismatch); !errors.Is(err, railcontract.ErrEvidenceConflict) {
			t.Fatal("accepted cross-program original")
		}
	}
	changedCfg := cfg
	changedCfg.Merchant = "otherMerchant"
	reassigned, err := New(changedCfg, db)
	if err != nil {
		t.Fatal(err)
	}
	reassignedCommand := cmd
	reassignedCommand.Merchant = changedCfg.Merchant
	if _, err := reassigned.Identity(context.Background(), reassignedCommand); !errors.Is(err, railcontract.ErrProcessorIdentityMismatch) {
		t.Fatal("re-enrolled existing source")
	}
	foreign := cmd
	foreign.ProviderAccount = "other"
	if _, _, err := c.Lookup(ctx, foreign); err == nil {
		t.Fatal("foreign account accepted")
	}
	badCtx := railcontract.WithProcessorIdentity(ctx, strings.Repeat("0", 64))
	if err = c.Submit(badCtx, cmd); !errors.Is(err, railcontract.ErrProcessorIdentityMismatch) {
		t.Fatal("rebound ledger")
	}
	live := cfg
	t.Run("merchant-gateway-to-Antom", func(t *testing.T) { exerciseGateway(t, cfg, base, fx) })
	live.ClientID = "LIVE_MERCHANT"
	if _, err := New(live, db); err == nil {
		t.Fatal("accepted live credential")
	}
}
func exerciseGateway(t *testing.T, cfg Config, base gwstore.Store, peer *sandboxFixture) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	domain := fmt.Sprintf("test/e2e-antom/%d/", now.UnixNano())
	adapter, err := New(cfg, fixtureStore{base, domain + "connector/"})
	if err != nil {
		t.Fatal(err)
	}
	adapter.http.Transport = peer
	adapter.now = func() time.Time { return peer.now }
	peer.payments = 0
	peer.refunds = 0
	peer.paid = false
	peer.fault = ""
	route := aggregation.Route{Merchant: cfg.Merchant, Account: cfg.GatewayAccount, Connector: cfg.ConnectorID, ID: cfg.RouteID, ProviderAccount: cfg.MerchantAccountID,
		Currency: cfg.Currency, MinAmount: 1, MaxAmount: 100000, Priority: 1, Enabled: true, Methods: []string{"alipay"}, Channels: []string{"hosted"}}
	router, err := aggregation.New([]aggregation.Route{route}, map[string]railcontract.Processor{cfg.ConnectorID: adapter}, nil)
	if err != nil {
		t.Fatal(err)
	}
	service := gateway.New(fixtureStore{base, domain + "gateway/"}, router)
	service.Now = func() time.Time { return now }
	service.RouteCandidates = router.RouteCandidates
	service.RouteExists = router.RouteExists
	service.CanDispatch = router.CanDispatch
	handler := service.Handler()
	user := gateway.Principal{Merchant: cfg.Merchant, Account: cfg.GatewayAccount, Beneficiary: "merchant-bank", Currency: cfg.Currency, Role: gateway.MerchantRole,
		Subject: "cashier", Outlets: map[string]bool{"o1": true}, MaxAmount: 100000}
	call := func(method, path, key, body string, code int) map[string]any {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(gateway.WithPrincipal(ctx, user))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != code {
			t.Fatalf("%s %s returned %d %s", method, path, rec.Code, rec.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	result := call("POST", "/v2/payment-intents", "pos-004", `{"order_id":"basket-web","allocation_id":"wallet","outlet_id":"o1","channel":"hosted","payment_method":"alipay","amount_minor":1234,"currency":"USD"}`, 201)
	path := "/v2/payment-intents/" + result["id"].(string)
	if err := service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if r := call("GET", path, "", "", 200); r["status"] != "pending" || r["checkout_url"] == nil {
		t.Fatalf("hosted presentation unavailable: %v", r)
	}
	now = now.Add(2 * time.Minute)
	peer.paid = true
	if err := service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if r := call("GET", path, "", "", 200); r["status"] != "captured" || r["reference"] != "pay123" {
		t.Fatalf("capture missing: %v", r)
	}
	if r := call("GET", path+"/release-evidence", "", "", 200); r["release_eligible"] != false || r["complete"] != false {
		t.Fatalf("retail evidence unexpectedly complete: %v", r)
	}
	ret := call("POST", path+"/refunds", "rt-004", `{"merchant_refund_id":"ret-web","amount_minor":800}`, 201)
	if err := service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if r := call("GET", "/v2/refunds/"+ret["id"].(string), "", "", 200); r["status"] != "refunded" || r["reference"] != "refund124" {
		t.Fatalf("refund missing: %v", r)
	}
	if peer.payments != 1 || peer.refunds != 1 {
		t.Fatal("unexpected external financial POST count")
	}
}

func TestSandboxRedirectPolicy(t *testing.T) {
	for _, address := range []string{"http://render.alipay.com/abc", "https://render.alipay.com.example.com/x", "https://render.alipay.com@invalid.example/x", "https://ac.alipay.com/live/x", "https://global.alipay.com:8443/x"} {
		if CheckoutURL(address) {
			t.Fatal("accepted off-boundary URL", address)
		}
	}
}

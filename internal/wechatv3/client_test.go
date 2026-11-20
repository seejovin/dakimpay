package wechatv3

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

type peer struct {
	t                  *testing.T
	merchant, platform *rsa.PrivateKey
	server             *httptest.Server
	respond            func(*http.Request, []byte) (int, string, func(http.Header))
}

func newPeer(t *testing.T) (*peer, *Client) {
	t.Helper()
	p := &peer{t: t}
	var err error
	p.merchant, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.platform, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.server.Close)
	c, err := NewLocal(p.server.URL, Institution{App: "wxMimi123", Merchant: "10001", SubMerchant: "20002"}, "AAA111", p.merchant, map[string]*rsa.PublicKey{"BBB222": &p.platform.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return p, c
}

var authPart = regexp.MustCompile(`^([a-z_]+)="([^"]+)"$`)

// The peer reconstructs the signed path and JSON from bytes actually received
// on HTTP. It never calls the client's signing or response-verification code.
func (p *peer) serve(w http.ResponseWriter, r *http.Request) {
	t := p.t
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
		return
	}
	prefix := "WECHATPAY2-SHA256-RSA2048 "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		t.Error("missing institutional signature")
		return
	}
	fields := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(h, prefix), ",") {
		m := authPart.FindStringSubmatch(part)
		if len(m) != 3 || fields[m[1]] != "" {
			t.Error("invalid authorization field")
			return
		}
		fields[m[1]] = m[2]
	}
	if len(fields) != 5 || fields["mchid"] != "10001" || fields["serial_no"] != "AAA111" || len(fields["nonce_str"]) != 32 {
		t.Error("invalid signing enrollment")
		return
	}
	reqTime, err := strconv.ParseInt(fields["timestamp"], 10, 64)
	if err != nil || time.Since(time.Unix(reqTime, 0)) > 5*time.Minute {
		t.Error("invalid request time")
		return
	}
	requestText := strings.Join([]string{r.Method, r.RequestURI, fields["timestamp"], fields["nonce_str"], string(raw), ""}, "\n")
	digest := sha256.Sum256([]byte(requestText))
	sig, err := base64.StdEncoding.DecodeString(fields["signature"])
	if err != nil || rsa.VerifyPKCS1v15(&p.merchant.PublicKey, crypto.SHA256, digest[:], sig) != nil {
		t.Error("exact-wire request signature mismatch")
		return
	}
	status, body, alter := p.respond(r, raw)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "localRun123"
	hash := sha256.Sum256([]byte(ts + "\n" + nonce + "\n" + body + "\n"))
	signature, err := rsa.SignPKCS1v15(rand.Reader, p.platform, crypto.SHA256, hash[:])
	if err != nil {
		t.Error(err)
		return
	}
	w.Header().Set("Wechatpay-Serial", "BBB222")
	w.Header().Set("Wechatpay-Timestamp", ts)
	w.Header().Set("Wechatpay-Nonce", nonce)
	w.Header().Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(signature))
	if alter != nil {
		alter(w.Header())
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

var original = Order{Number: "DKP001", Description: "Mimi - Taipei - groceries", MCC: "5411", NotifyURL: "https://gateway.example.invalid/wechat", Amount: Amount{Total: 1234, Currency: "HKD"}}

const paid = `{"sp_appid":"wxMimi123","sp_mchid":"10001","sub_mchid":"20002","out_trade_no":"DKP001","id":"420001","trade_type":"NATIVE","trade_state":"SUCCESS","success_time":"2026-11-19T10:35:40+08:00","amount":{"total":1234,"currency":"HKD","payer_currency":"CNY","payer_total":1011}}`
const refunded = `{"id":"500001","out_refund_no":"REF001","transaction_id":"420001","out_trade_no":"DKP001","status":"SUCCESS","success_time":"2026-11-19T11:03:40+08:00","amount":{"refund":200,"currency":"HKD","payer_currency":"CNY","payer_refund":170}}`

func assertJSON(t *testing.T, raw []byte, want string) {
	t.Helper()
	var a, b any
	if json.Unmarshal(raw, &a) != nil || json.Unmarshal([]byte(want), &b) != nil || !reflect.DeepEqual(a, b) {
		t.Errorf("JSON differs: %s", raw)
	}
}
func TestInstitutionalNativePaymentAndRefund(t *testing.T) {
	p, c := newPeer(t)
	seen := map[string]bool{}
	p.respond = func(r *http.Request, raw []byte) (int, string, func(http.Header)) {
		k := r.Method + " " + r.RequestURI
		seen[k] = true
		switch k {
		case "POST /v3/global/transactions/native":
			assertJSON(t, raw, `{"sp_appid":"wxMimi123","sp_mchid":"10001","sub_mchid":"20002","out_trade_no":"DKP001","description":"Mimi - Taipei - groceries","merchant_category_code":"5411","notify_url":"https://gateway.example.invalid/wechat","trade_type":"NATIVE","amount":{"total":1234,"currency":"HKD"}}`)
			return 200, `{"code_url":"weixin://wxpay/s/Testing"}`, nil
		case "GET /v3/global/transactions/out-trade-no/DKP001?sp_mchid=10001&sub_mchid=20002":
			if len(raw) != 0 {
				t.Error("GET with body")
			}
			return 200, paid, nil
		case "POST /v3/global/refunds":
			assertJSON(t, raw, `{"sp_appid":"wxMimi123","sp_mchid":"10001","sub_mchid":"20002","transaction_id":"420001","out_refund_no":"REF001","amount":{"total":1234,"refund":200,"currency":"HKD"}}`)
			return 200, `{"id":"500001","out_refund_no":"REF001","create_time":"2026-11-19T10:55:40+08:00","amount":{"refund":200,"currency":"HKD"}}`, nil
		case "GET /v3/global/refunds/out-refund-no/REF001?sp_mchid=10001&sub_mchid=20002":
			return 200, refunded, nil
		default:
			t.Error("unexpected API:", k)
			return 400, `{}`, nil
		}
	}
	qr, err := c.CreateNative(t.Context(), original)
	if err != nil || qr != "weixin://wxpay/s/Testing" {
		t.Fatal(qr, err)
	}
	parent, err := c.Query(t.Context(), original)
	if err != nil || parent.Transaction != "420001" {
		t.Fatal(parent, err)
	}
	r := Refund{Number: "REF001", Minor: 200}
	changed := parent
	changed.Amount.Total++
	if _, err = c.RequestRefund(t.Context(), changed, r); err == nil {
		t.Fatal("edited query evidence accepted")
	}
	receipt, err := c.RequestRefund(t.Context(), parent, r)
	if err != nil || receipt != "500001" {
		t.Fatal(receipt, err)
	}
	f, err := c.QueryRefund(t.Context(), parent, r, receipt)
	if err != nil || f.State != "SUCCESS" {
		t.Fatal(f, err)
	}
	if len(seen) != 4 {
		t.Fatal("missing workflow stage")
	}
}

func TestSignedResponsesFailClosed(t *testing.T) {
	p, c := newPeer(t)
	for _, tc := range []struct {
		name, body string
		mutate     func(http.Header)
	}{
		{"cross_account", strings.Replace(paid, "20002", "20003", 1), nil},
		{"other_order", strings.Replace(paid, "DKP001", "DKP002", 1), nil},
		{"wrong_currency", strings.Replace(paid, "HKD", "USD", 1), nil},
		{"wrong_amount", strings.Replace(paid, "1234", "1235", 1), nil},
		{"wrong_product", strings.Replace(paid, "NATIVE", "JSAPI", 1), nil},
		{"future_state", strings.Replace(paid, "SUCCESS", "WAIT_OTHER", 1), nil},
		{"duplicate", strings.Replace(paid, `"total":1234`, `"total":1235,"total":1234`, 1), nil},
		{"case_alias", strings.Replace(paid, `"total":1234`, `"Total":1235,"total":1234`, 1), nil},
		{"untrusted_serial", paid, func(h http.Header) { h.Set("Wechatpay-Serial", "BBB223") }},
		{"bad_signature", paid, func(h http.Header) { h.Set("Wechatpay-Signature", "AAAA") }},
		{"double_nonce", paid, func(h http.Header) { h.Add("Wechatpay-Nonce", "other") }},
		{"missing_serial", paid, func(h http.Header) { h.Del("Wechatpay-Serial") }},
		{"algorithm", paid, func(h http.Header) { h.Set("Wechatpay-Signature-Type", "other") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.respond = func(*http.Request, []byte) (int, string, func(http.Header)) { return 200, tc.body, tc.mutate }
			if result, err := c.Query(t.Context(), original); err == nil || result.Transaction != "" {
				t.Fatal("unsafe result accepted")
			}
		})
	}
	for _, offset := range []int64{-301, 301} {
		t.Run("freshness_"+strconv.FormatInt(offset, 10), func(t *testing.T) {
			now := time.Now()
			ts := strconv.FormatInt(now.Unix()+offset, 10)
			hash := sha256.Sum256([]byte(ts + "\nnonce123\n" + paid + "\n"))
			signature, _ := rsa.SignPKCS1v15(rand.Reader, p.platform, crypto.SHA256, hash[:])
			h := http.Header{}
			h.Set("Wechatpay-Serial", "BBB222")
			h.Set("Wechatpay-Nonce", "nonce123")
			h.Set("Wechatpay-Timestamp", ts)
			h.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(signature))
			if c.verify(h, []byte(paid), now) == nil {
				t.Fatal("stale signed response accepted")
			}
		})
	}
	p.respond = func(*http.Request, []byte) (int, string, func(http.Header)) { return 200, paid, nil }
	parent, err := c.Query(t.Context(), original)
	if err != nil {
		t.Fatal(err)
	}
	for _, from := range []string{"420001", "DKP001", "REF001", "500001", "SUCCESS", "200,"} {
		p.respond = func(*http.Request, []byte) (int, string, func(http.Header)) {
			return 200, strings.Replace(refunded, from, "999", 1), nil
		}
		if _, err = c.QueryRefund(t.Context(), parent, Refund{"REF001", 200}, "500001"); err == nil {
			t.Fatal("refund changed:", from)
		}
	}
	for _, origin := range []string{"https://apihk.mch.weixin.qq.com", "http://127.0.0.1:4567?q=x", "http://127.0.0.1:80/other", "http://localhost:4444", "http://192.0.2.1:8080"} {
		if _, err := NewLocal(origin, c.institution, c.merchantSerial, p.merchant, c.trusted); err == nil {
			t.Fatal("noncanonical local origin accepted")
		}
	}
}

func TestInconclusiveEvidenceAndProcessingRefund(t *testing.T) {
	p, c := newPeer(t)
	queries := 0
	p.respond = func(r *http.Request, raw []byte) (int, string, func(http.Header)) {
		queries++
		return http.StatusNotFound, `{"code":"ORDER_NOT_EXIST","message":"unavailable original"}`, nil
	}
	state, err := c.Query(t.Context(), original)
	if err == nil || state != (Payment{}) || queries != 1 {
		t.Fatal("HTTP error became payment evidence or automatically retried")
	}
	p.respond = func(*http.Request, []byte) (int, string, func(http.Header)) {
		return http.StatusOK, strings.Replace(paid, `"SUCCESS"`, `"USERPAYING"`, 1), nil
	}
	state, err = c.Query(t.Context(), original)
	if err != nil || state.State != "USERPAYING" {
		t.Fatal(state, err)
	}
	if _, err = c.RequestRefund(t.Context(), state, Refund{Number: "REF001", Minor: 200}); err == nil {
		t.Fatal("refund accepted on nonterminal payment")
	}
	p.respond = func(*http.Request, []byte) (int, string, func(http.Header)) { return http.StatusOK, paid, nil }
	parent, err := c.Query(t.Context(), original)
	if err != nil {
		t.Fatal(err)
	}
	p.respond = func(*http.Request, []byte) (int, string, func(http.Header)) {
		return http.StatusOK, strings.Replace(refunded, `"SUCCESS"`, `"PROCESSING"`, 1), nil
	}
	refund, err := c.QueryRefund(t.Context(), parent, Refund{Number: "REF001", Minor: 200}, "")
	if err != nil || refund.State != "PROCESSING" {
		t.Fatal(refund, err)
	}
}

// Package antomtest implements the Antom online sandbox for an Alipay wallet.
package antomtest

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
)

const sandboxPath = "/ams/sandbox/api/v1/payments/"

var regions = map[string]string{
	"asia":                "open-sea-global.alipay.com",
	"north-america-nonus": "open-na-global.alipay.com",
	"europe":              "open-de-global.alipay.com",
}
var clientID = regexp.MustCompile(`^SANDBOX_[a-zA-Z0-9]{4,60}$`)
var accountID = regexp.MustCompile(`^[a-zA-Z0-9]{1,32}$`)
var version = regexp.MustCompile(`^[1-9][0-9]{0,7}$`)
var remoteID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

type Config struct {
	Merchant              string `json:"merchant"`
	GatewayAccount        string `json:"gateway_account"`
	ConnectorID           string `json:"connector_id"`
	RouteID               string `json:"route_id"`
	Currency              string `json:"currency"`
	ClientID              string `json:"client_id"`
	MerchantAccountID     string `json:"merchant_account_id"`
	MerchantPrivateKeyPEM string `json:"merchant_private_key_pem"`
	AntomPublicKeyPEM     string `json:"antom_public_key_pem"`
	MerchantKeyVersion    string `json:"merchant_key_version"`
	AntomKeyVersion       string `json:"antom_key_version"`
	Region                string `json:"region"`
	RedirectURL           string `json:"redirect_url"`
}

type scope struct{ Merchant, GatewayAccount, ConnectorID, RouteID, Currency string }
type Client struct {
	scope                                                      scope
	clientID, account, host, redirect, keyVersion, peerVersion string
	private                                                    *rsa.PrivateKey
	public                                                     *rsa.PublicKey
	db                                                         gwstore.Store
	http                                                       *http.Client
	now                                                        func() time.Time
}

func New(cfg Config, store gwstore.Store) (*Client, error) {
	host, ok := regions[cfg.Region]
	redirect, err := url.Parse(cfg.RedirectURL)
	if !ok || !idPattern.MatchString(cfg.Merchant) || !idPattern.MatchString(cfg.GatewayAccount) ||
		!idPattern.MatchString(cfg.ConnectorID) || !idPattern.MatchString(cfg.RouteID) || !currency.MatchString(cfg.Currency) ||
		store == nil || !clientID.MatchString(cfg.ClientID) || !accountID.MatchString(cfg.MerchantAccountID) ||
		!version.MatchString(cfg.MerchantKeyVersion) || !version.MatchString(cfg.AntomKeyVersion) ||
		err != nil || redirect.Scheme != "https" || redirect.Hostname() == "" || redirect.User != nil || redirect.Fragment != "" || len(cfg.RedirectURL) > 2048 {
		return nil, errors.New("invalid Antom test enrollment")
	}
	privBlock, rest := pem.Decode([]byte(cfg.MerchantPrivateKeyPEM))
	if privBlock == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("missing RSA merchant signing key")
	}
	var private *rsa.PrivateKey
	switch privBlock.Type {
	case "RSA PRIVATE KEY":
		private, err = x509.ParsePKCS1PrivateKey(privBlock.Bytes)
	case "PRIVATE KEY":
		var key any
		key, err = x509.ParsePKCS8PrivateKey(privBlock.Bytes)
		private, _ = key.(*rsa.PrivateKey)
	}
	if err != nil || private == nil || private.N.BitLen() < 2048 || private.N.BitLen() > 4096 || private.Validate() != nil {
		return nil, errors.New("invalid merchant RSA signing key")
	}
	block, rest := pem.Decode([]byte(cfg.AntomPublicKeyPEM))
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("missing Antom response verification key")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	public, ok := k.(*rsa.PublicKey)
	if err != nil || !ok || public.N.BitLen() < 2048 || public.N.BitLen() > 4096 || public.Equal(&private.PublicKey) {
		return nil, errors.New("invalid or reused Antom response verification key")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &Client{scope: scope{cfg.Merchant, cfg.GatewayAccount, cfg.ConnectorID, cfg.RouteID, cfg.Currency}, clientID: cfg.ClientID, account: cfg.MerchantAccountID, host: host, redirect: cfg.RedirectURL, keyVersion: cfg.MerchantKeyVersion,
		peerVersion: cfg.AntomKeyVersion, private: private, public: public, db: store, now: time.Now, http: &http.Client{Transport: tr, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// CheckoutURL allows only HTTPS authorities used in the documented Alipay
// normalUrl examples. New wallet domains require explicit qualification.
func CheckoutURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= 2048 && u.Scheme == "https" && u.User == nil && u.Fragment == "" && u.Path != "" &&
		(u.Host == "render.alipay.com" || u.Host == "global.alipay.com" || (u.Host == "ac.alipay.com" && u.EscapedPath() == "/page/sandbox/stone-infostaging.html"))
}

type money struct {
	Currency string `json:"currency"`
	Value    string `json:"value"`
}
type result struct {
	Status string `json:"resultStatus"`
	Code   string `json:"resultCode"`
}
type response struct {
	Result            result `json:"result"`
	PaymentRequestID  string `json:"paymentRequestId"`
	PaymentID         string `json:"paymentId"`
	PaymentStatus     string `json:"paymentStatus"`
	PaymentAmount     money  `json:"paymentAmount"`
	RefundRequestID   string `json:"refundRequestId"`
	RefundID          string `json:"refundId"`
	RefundStatus      string `json:"refundStatus"`
	RefundAmount      money  `json:"refundAmount"`
	MerchantAccountID string `json:"merchantAccountId"`
	NormalURL         string `json:"normalUrl"`
	Metadata          string `json:"metadata"`
}

func signingDigest(path, id, stamp string, body []byte) []byte {
	h := sha256.New()
	fmt.Fprintf(h, "POST %s\n%s.%s.", path, id, stamp)
	h.Write(body)
	return h.Sum(nil)
}

func (c *Client) post(ctx context.Context, action string, input any) (response, time.Time, error) {
	var out response
	var timestamp time.Time
	switch action {
	case "pay", "refund", "inquiryPayment", "inquiryRefund":
	default:
		return out, timestamp, errors.New("invalid Antom operation")
	}
	body, err := json.Marshal(input)
	if err != nil {
		return out, timestamp, err
	}
	path := sandboxPath + action
	sent := c.now().UTC()
	stamp := sent.Format(time.RFC3339Nano)
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.private, crypto.SHA256, signingDigest(path, c.clientID, stamp, body))
	if err != nil {
		return out, timestamp, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+c.host+path, bytes.NewReader(body))
	if err != nil {
		return out, timestamp, err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Client-Id", c.clientID)
	req.Header.Set("Request-Time", stamp)
	req.Header.Set("Signature", "algorithm=RSA256,keyVersion="+c.keyVersion+",signature="+url.QueryEscape(base64.StdEncoding.EncodeToString(sig)))
	reply, err := c.http.Do(req)
	if err != nil {
		return out, timestamp, errors.New("Antom sandbox transport unavailable")
	}
	defer reply.Body.Close()
	content, err := io.ReadAll(io.LimitReader(reply.Body, (256<<10)+1))
	if err != nil || len(content) > 256<<10 || reply.StatusCode != http.StatusOK {
		return out, timestamp, errors.New("no verified Antom sandbox reply")
	}
	one := func(name string) string {
		v := reply.Header.Values(name)
		if len(v) != 1 {
			return ""
		}
		return v[0]
	}
	id, when, header := one("Client-Id"), one("Response-Time"), one("Signature")
	if id != c.clientID || len(header) > 4096 {
		return out, timestamp, railcontract.ErrEvidenceConflict
	}
	timestamp, err = time.Parse(time.RFC3339Nano, when)
	if err != nil && len(when) == 13 {
		if millis, e := strconv.ParseInt(when, 10, 64); e == nil {
			timestamp = time.UnixMilli(millis)
			err = nil
		}
	}
	if err != nil || timestamp.Before(sent.Add(-2*time.Minute)) || timestamp.After(c.now().Add(2*time.Minute)) {
		return out, timestamp, railcontract.ErrEvidenceConflict
	}
	parts := map[string]string{}
	for _, p := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || parts[k] != "" || v == "" {
			return out, timestamp, railcontract.ErrEvidenceConflict
		}
		parts[k] = v
	}
	if len(parts) != 3 || parts["algorithm"] != "RSA256" || parts["keyVersion"] != c.peerVersion {
		return out, timestamp, railcontract.ErrEvidenceConflict
	}
	encoded, err := url.QueryUnescape(parts["signature"])
	if err != nil {
		return out, timestamp, railcontract.ErrEvidenceConflict
	}
	proof, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || rsa.VerifyPKCS1v15(c.public, crypto.SHA256, signingDigest(path, id, when, content), proof) != nil {
		return out, timestamp, railcontract.ErrEvidenceConflict
	}
	if !unambiguous(content) || json.Unmarshal(content, &out) != nil || out.Result.Code == "" || (out.MerchantAccountID != "" && out.MerchantAccountID != c.account) {
		return response{}, timestamp, railcontract.ErrEvidenceConflict
	}
	return out, timestamp, nil
}

// Permit additive response fields while rejecting repeated or aliased economic
// fields. Verification above uses the original wire bytes including whitespace.
func unambiguous(body []byte) bool {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var visit func(int) bool
	visit = func(depth int) bool {
		if depth > 20 {
			return false
		}
		tok, err := d.Token()
		if err != nil {
			return false
		}
		switch tok {
		case json.Delim('{'):
			keys := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				s, ok := k.(string)
				low := strings.ToLower(s)
				if err != nil || !ok || keys[low] {
					return false
				}
				keys[low] = true
				if canon, ok := exactFields[low]; ok && s != canon {
					return false
				}
				if !visit(depth + 1) {
					return false
				}
			}
			_, err = d.Token()
			return err == nil
		case json.Delim('['):
			for d.More() {
				if !visit(depth + 1) {
					return false
				}
			}
			_, err = d.Token()
			return err == nil
		default:
			return true
		}
	}
	if !visit(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

var exactFields = func() map[string]string {
	m := map[string]string{}
	for _, n := range []string{"result", "resultStatus", "resultCode", "paymentRequestId", "paymentId", "paymentStatus", "paymentAmount", "refundRequestId", "refundId", "refundStatus", "refundAmount", "merchantAccountId", "normalUrl", "currency", "value", "metadata"} {
		m[strings.ToLower(n)] = n
	}
	return m
}()

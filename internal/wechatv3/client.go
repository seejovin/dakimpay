// Package wechatv3 qualifies the cross-border institutional Native payment wire
// contract against an independently signed loopback peer. It has no live host.
package wechatv3

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const algorithm = "WECHATPAY2-SHA256-RSA2048"

var (
	id          = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	refundID    = regexp.MustCompile(`^[A-Za-z0-9_|*@-]{1,64}$`)
	serial      = regexp.MustCompile(`^[0-9A-F]{1,64}$`)
	iso         = regexp.MustCompile(`^[A-Z]{3}$`)
	mcc         = regexp.MustCompile(`^[0-9]{4}$`)
	headerNonce = regexp.MustCompile(`^[[:graph:]]{1,128}$`)
	payerCode   = regexp.MustCompile(`^weixin://wxpay/[A-Za-z0-9_/?=&%-]+$`)
)

type Institution struct {
	App         string `json:"sp_appid"`
	Merchant    string `json:"sp_mchid"`
	SubMerchant string `json:"sub_mchid"`
}
type Client struct {
	origin         string
	institution    Institution
	merchantKey    *rsa.PrivateKey
	merchantSerial string
	trusted        map[string]*rsa.PublicKey
	http           *http.Client
}

// NewLocal accepts a pinned set of independently provisioned platform public
// keys. Its origin, HTTP transport and redirects cannot select a remote host.
func NewLocal(origin string, program Institution, merchantSerial string, merchantKey *rsa.PrivateKey, platformKeys map[string]*rsa.PublicKey) (*Client, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Port() == "" || origin != "http://127.0.0.1:"+u.Port() {
		return nil, errors.New("explicit loopback test origin required")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid loopback port")
	}
	if !id.MatchString(program.App) || !id.MatchString(program.Merchant) || !id.MatchString(program.SubMerchant) || program.Merchant == program.SubMerchant {
		return nil, errors.New("invalid institutional account scope")
	}
	if !serial.MatchString(merchantSerial) || merchantKey == nil || merchantKey.N == nil || merchantKey.N.BitLen() != 2048 || merchantKey.Validate() != nil || len(platformKeys) == 0 {
		return nil, errors.New("invalid merchant signing identity")
	}
	keys := make(map[string]*rsa.PublicKey, len(platformKeys))
	for s, k := range platformKeys {
		if !serial.MatchString(s) || k == nil || k.N == nil || k.N.BitLen() != 2048 || k.E != 65537 || k.N.Cmp(merchantKey.N) == 0 {
			return nil, errors.New("invalid or overlapping platform trust")
		}
		keys[s] = k
	}
	return &Client{origin: origin, institution: program, merchantSerial: merchantSerial, merchantKey: merchantKey, trusted: keys, http: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil, DisableCompression: true, MaxResponseHeaderBytes: 16384}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

type Amount struct {
	Total    int64  `json:"total"`
	Currency string `json:"currency"`
}
type Order struct {
	Number      string `json:"out_trade_no"`
	Description string `json:"description"`
	MCC         string `json:"merchant_category_code"`
	NotifyURL   string `json:"notify_url"`
	Amount      Amount `json:"amount"`
}

func (o Order) valid() bool {
	u, err := url.Parse(o.NotifyURL)
	return id.MatchString(o.Number) && utf8.ValidString(o.Description) && strings.TrimSpace(o.Description) != "" && len(o.Description) <= 128 && mcc.MatchString(o.MCC) &&
		err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && len(o.NotifyURL) <= 256 &&
		o.Amount.Total > 0 && o.Amount.Total <= 2147483647 && iso.MatchString(o.Amount.Currency)
}

// CreateNative returns the provider's QR payload, which conveys no payment result.
func (c *Client) CreateNative(ctx context.Context, original Order) (string, error) {
	if !original.valid() {
		return "", errors.New("invalid original native payment terms")
	}
	in := struct {
		Institution
		Order
		TradeType string `json:"trade_type"`
	}{c.institution, original, "NATIVE"}
	var out struct {
		Code string `json:"code_url"`
	}
	if err := c.exchange(ctx, http.MethodPost, "/v3/global/transactions/native", in, &out); err != nil {
		return "", err
	}
	if len(out.Code) > 64 || !payerCode.MatchString(out.Code) {
		return "", errors.New("invalid native QR payload")
	}
	return out.Code, nil
}

type paymentProof struct {
	client   *Client
	snapshot Payment
}
type Payment struct {
	proof *paymentProof
	Institution
	Order       string `json:"out_trade_no"`
	Transaction string `json:"id"`
	State       string `json:"trade_state"`
	TradeType   string `json:"trade_type"`
	Amount      Amount `json:"amount"`
	SuccessTime string `json:"success_time"`
}

func (c *Client) queryScope() string {
	return url.Values{"sp_mchid": {c.institution.Merchant}, "sub_mchid": {c.institution.SubMerchant}}.Encode()
}

// Query requires the complete expected identity and priced amount before a
// caller can observe any state. A failed query conveys no evidence of absence.
func (c *Client) Query(ctx context.Context, original Order) (Payment, error) {
	var out Payment
	if !original.valid() {
		return out, errors.New("invalid original native payment terms")
	}
	path := "/v3/global/transactions/out-trade-no/" + url.PathEscape(original.Number) + "?" + c.queryScope()
	if err := c.exchange(ctx, http.MethodGet, path, nil, &out); err != nil {
		return Payment{}, err
	}
	if out.Institution != c.institution || out.Order != original.Number || out.Amount != original.Amount || out.TradeType != "NATIVE" {
		return Payment{}, errors.New("query evidence differs from original order")
	}
	switch out.State {
	case "SUCCESS", "REFUND":
		_, err := time.Parse(time.RFC3339, out.SuccessTime)
		if !id.MatchString(out.Transaction) || err != nil {
			return Payment{}, errors.New("missing original successful payment identity")
		}
	case "NOTPAY", "CLOSED", "USERPAYING", "PAYERROR":
	default:
		return Payment{}, errors.New("unsupported native order state")
	}
	out.proof = &paymentProof{client: c, snapshot: out}
	return out, nil
}

type Refund struct {
	Number string
	Minor  int64
}
type refundAmount struct {
	Refund   int64  `json:"refund"`
	Currency string `json:"currency"`
}
type RefundObservation struct {
	ID          string       `json:"id"`
	Number      string       `json:"out_refund_no"`
	Order       string       `json:"out_trade_no"`
	Transaction string       `json:"transaction_id"`
	State       string       `json:"status"`
	Amount      refundAmount `json:"amount"`
	SuccessTime string       `json:"success_time"`
}

func (c *Client) originalRefund(parent Payment, refund Refund) error {
	proof := parent.proof
	parent.proof = nil
	if proof == nil || proof.client != c || proof.snapshot != parent {
		return errors.New("refund requires the verified original query from this client")
	}

	if (parent.State != "SUCCESS" && parent.State != "REFUND") || parent.Institution != c.institution || parent.TradeType != "NATIVE" || !id.MatchString(parent.Order) || !id.MatchString(parent.Transaction) || !iso.MatchString(parent.Amount.Currency) || parent.Amount.Total > 2147483647 || !refundID.MatchString(refund.Number) || refund.Minor < 1 || refund.Minor > parent.Amount.Total {
		return errors.New("invalid original refund terms")
	}
	return nil
}

// RequestRefund returns an acceptance ID only. QueryRefund is required to
// establish the independently observed status of that original refund.
func (c *Client) RequestRefund(ctx context.Context, parent Payment, r Refund) (string, error) {
	if err := c.originalRefund(parent, r); err != nil {
		return "", err
	}
	in := struct {
		Institution
		Transaction string `json:"transaction_id"`
		Number      string `json:"out_refund_no"`
		Amount      struct {
			Amount
			Refund int64 `json:"refund"`
		} `json:"amount"`
	}{Institution: c.institution, Transaction: parent.Transaction, Number: r.Number}
	in.Amount.Amount = parent.Amount
	in.Amount.Refund = r.Minor
	var out RefundObservation
	if err := c.exchange(ctx, http.MethodPost, "/v3/global/refunds", in, &out); err != nil {
		return "", err
	}
	if !id.MatchString(out.ID) || out.Number != r.Number || out.Amount != (refundAmount{r.Minor, parent.Amount.Currency}) {
		return "", errors.New("refund acceptance differs from original instruction")
	}
	return out.ID, nil
}

func (c *Client) QueryRefund(ctx context.Context, parent Payment, r Refund, acceptedID string) (RefundObservation, error) {
	if err := c.originalRefund(parent, r); err != nil {
		return RefundObservation{}, err
	}
	path := "/v3/global/refunds/out-refund-no/" + url.PathEscape(r.Number) + "?" + c.queryScope()
	var out RefundObservation
	if err := c.exchange(ctx, http.MethodGet, path, nil, &out); err != nil {
		return RefundObservation{}, err
	}
	if !id.MatchString(out.ID) || (acceptedID != "" && out.ID != acceptedID) || out.Number != r.Number || out.Order != parent.Order || out.Transaction != parent.Transaction || out.Amount != (refundAmount{r.Minor, parent.Amount.Currency}) {
		return RefundObservation{}, errors.New("refund inquiry differs from original instruction")
	}
	switch out.State {
	case "SUCCESS":
		if _, err := time.Parse(time.RFC3339, out.SuccessTime); err != nil {
			return RefundObservation{}, errors.New("missing successful refund timestamp")
		}
	case "PROCESSING", "REFUNDCLOSE", "ABNORMAL":
	default:
		return RefundObservation{}, errors.New("unsupported original refund state")
	}
	return out, nil
}

func (c *Client) exchange(ctx context.Context, method, path string, body any, out any) error {
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.origin+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	var entropy [16]byte
	if _, err = rand.Read(entropy[:]); err != nil {
		return err
	}
	nonce := hex.EncodeToString(entropy[:])
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	message := method + "\n" + req.URL.RequestURI() + "\n" + ts + "\n" + nonce + "\n" + string(raw) + "\n"
	hash := sha256.Sum256([]byte(message))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.merchantKey, crypto.SHA256, hash[:])
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf(`%s mchid="%s",nonce_str="%s",signature="%s",timestamp="%s",serial_no="%s"`, algorithm, c.institution.Merchant, nonce, base64.StdEncoding.EncodeToString(sig), ts, c.merchantSerial))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	const max = 128 << 10
	reply, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil || len(reply) > max {
		return errors.New("invalid or oversized WeChat Pay response")
	}
	if err = c.verify(resp.Header, reply, time.Now()); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("signed WeChat Pay HTTP response %d; original outcome unresolved", resp.StatusCode)
	}
	if err = checkObject(reply); err != nil {
		return err
	}
	return json.Unmarshal(reply, out)
}

func (c *Client) verify(h http.Header, body []byte, now time.Time) error {
	for _, key := range []string{"Wechatpay-Serial", "Wechatpay-Timestamp", "Wechatpay-Nonce", "Wechatpay-Signature"} {
		if len(h.Values(key)) != 1 {
			return errors.New("missing or duplicate WeChat Pay signature header")
		}
	}
	if types := h.Values("Wechatpay-Signature-Type"); len(types) > 1 || (len(types) == 1 && types[0] != algorithm) {
		return errors.New("unknown platform signature type")
	}
	key := c.trusted[h.Get("Wechatpay-Serial")]
	ts, nonce := h.Get("Wechatpay-Timestamp"), h.Get("Wechatpay-Nonce")
	seconds, err := strconv.ParseInt(ts, 10, 64)
	if key == nil || err != nil || strconv.FormatInt(seconds, 10) != ts || seconds < now.Unix()-300 || seconds > now.Unix()+300 || !headerNonce.MatchString(nonce) {
		return errors.New("untrusted or stale WeChat Pay envelope")
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(h.Get("Wechatpay-Signature"))
	if err != nil {
		return errors.New("invalid platform signature encoding")
	}
	hash := sha256.Sum256([]byte(ts + "\n" + nonce + "\n" + string(body) + "\n"))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, hash[:], signature) != nil {
		return errors.New("invalid WeChat Pay response signature")
	}
	return nil
}

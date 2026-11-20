// Package partnerstripe qualifies a single merchant's Stripe test-mode Checkout account.
package partnerstripe

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"dakimpay/internal/railcontract"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	TestKey        string              `json:"test_key"`
	AccountID      string              `json:"account_id"`
	Merchant       string              `json:"merchant"`
	GatewayAccount string              `json:"gateway_account"`
	ConnectorID    string              `json:"connector_id"`
	ReturnURL      string              `json:"return_url"`
	AllowedRoutes  map[string][]string `json:"allowed_routes"`
	Capabilities   map[string][]string `json:"capabilities"`
}

type Adapter struct {
	cfg    Config
	db     *pgxpool.Pool
	http   *http.Client
	ledger string
}

var tokenID = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
var accountID = regexp.MustCompile(`^acct_[A-Za-z0-9]+$`)
var stripeID = regexp.MustCompile(`^[A-Za-z0-9_]{5,240}$`)
var currencyID = regexp.MustCompile(`^[A-Z]{3}$`)
var ErrUnknown = errors.New("Stripe sandbox original is unresolved; query or recover the original provider object")
var ErrConflict = railcontract.ErrEvidenceConflict

func (c Config) Validate() error {
	if !strings.HasPrefix(c.TestKey, "sk_test_") || len(c.TestKey) < 24 || !accountID.MatchString(c.AccountID) || !tokenID.MatchString(c.Merchant) || !tokenID.MatchString(c.GatewayAccount) || !tokenID.MatchString(c.ConnectorID) {
		return errors.New("expected a test secret and explicit Stripe sandbox account, gateway scope and connector")
	}
	u, err := url.Parse(c.ReturnURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("return_url must be a fixed merchant HTTPS URL without credentials, query or fragment")
	}
	if len(c.AllowedRoutes) == 0 || len(c.Capabilities) == 0 {
		return errors.New("explicit routes and capabilities required")
	}
	for route, methods := range c.AllowedRoutes {
		if !tokenID.MatchString(route) || len(methods) == 0 {
			return errors.New("invalid Stripe sandbox route")
		}
		seen := map[string]bool{}
		for _, m := range methods {
			if seen[m] || len(c.Capabilities[m]) == 0 {
				return errors.New("duplicate or unsupported route method")
			}
			seen[m] = true
		}
	}
	for m, cc := range c.Capabilities {
		if (m != "card" && m != "alipay" && m != "wechat_pay") || len(cc) == 0 {
			return errors.New("Stripe sandbox supports explicit hosted card, alipay and wechat_pay capabilities")
		}
		for _, cur := range cc {
			if !currencyID.MatchString(cur) {
				return errors.New("currencies must be uppercase ISO currency codes")
			}
		}
	}
	return nil
}

func New(ctx context.Context, db *pgxpool.Pool, c Config) (*Adapter, error) {
	// No configurable egress origin, proxy or redirect path for secret-bearing requests.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 4 * time.Second, IdleConnTimeout: 60 * time.Second}, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return newAdapter(ctx, db, c, client)
}

func newAdapter(ctx context.Context, db *pgxpool.Pool, c Config, client *http.Client) (*Adapter, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if db == nil {
		return nil, errors.New("dedicated Stripe sandbox PostgreSQL database required")
	}
	a := &Adapter{cfg: c, db: db, http: client}
	var acct struct{ Object, ID, Country string }
	if err := a.request(ctx, "GET", "/v1/account", nil, "", &acct); err != nil {
		return nil, err
	}
	if acct.Object != "account" || acct.ID != c.AccountID {
		return nil, railcontract.ErrProcessorIdentityMismatch
	}
	for method, currencies := range c.Capabilities {
		for _, cur := range currencies {
			if !eligible(acct.Country, method, cur) {
				return nil, fmt.Errorf("unsupported %s/%s for this Stripe account country", method, cur)
			}
		}
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	// One installation per database. Startup serialization also guards concurrent first provisioning.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(795642120281)"); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, ddl); err != nil {
		return nil, err
	}
	var seed [32]byte
	if _, err = rand.Read(seed[:]); err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO partnerstripe.installation(singleton,ledger,account,merchant,gateway_account,connector) VALUES(true,$1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, hex.EncodeToString(seed[:]), c.AccountID, c.Merchant, c.GatewayAccount, c.ConnectorID)
	if err != nil {
		return nil, err
	}
	err = tx.QueryRow(ctx, `SELECT ledger FROM partnerstripe.installation WHERE account=$1 AND merchant=$2 AND gateway_account=$3 AND connector=$4`, c.AccountID, c.Merchant, c.GatewayAccount, c.ConnectorID).Scan(&a.ledger)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, railcontract.ErrProcessorIdentityMismatch
	}
	if err != nil {
		return nil, err
	}
	return a, tx.Commit(ctx)
}

const ddl = `CREATE SCHEMA IF NOT EXISTS partnerstripe;
CREATE TABLE IF NOT EXISTS partnerstripe.installation (singleton boolean PRIMARY KEY CHECK (singleton), ledger text NOT NULL UNIQUE, account text NOT NULL, merchant text NOT NULL, gateway_account text NOT NULL, connector text NOT NULL);
CREATE TABLE IF NOT EXISTS partnerstripe.operation (id text PRIMARY KEY, ledger text NOT NULL REFERENCES partnerstripe.installation(ledger), terms jsonb NOT NULL, digest text NOT NULL, parent_charge text NOT NULL DEFAULT '', provider_id text UNIQUE, created_at timestamptz NOT NULL DEFAULT now());`

func eligible(country, method, cur string) bool {
	if method == "card" {
		return true
	} // Stripe still applies its account, method and currency restrictions.
	euro := "AT BE BG HR CY CZ DK EE FI FR DE GR HU IE IT LV LI LT LU MT NL NO PT RO SK SI ES SE CH"
	allowed := "AT AU BE BG CA CH CY CZ DE DK EE ES FI FR GB GI GR HK HR HU IE IT JP LI LT LU LV MT MY NL NO NZ PT RO SE SG SI SK US"
	if method == "wechat_pay" {
		euro = "AT BE DK FI FR DE IE IT LU NL NO PT ES SE CH"
		allowed = "CA US AT BE CH DE DK ES FI FR GB IE IT LU NL NO PT SE AU HK JP SG"
	}
	if !slices.Contains(strings.Fields(allowed), country) {
		return false
	}
	if cur == "CNY" {
		return true
	}
	if cur == "EUR" {
		return slices.Contains(strings.Fields(euro), country)
	}
	mapping := map[string]string{"AUD": "AU", "CAD": "CA", "GBP": "GB", "HKD": "HK", "JPY": "JP", "SGD": "SG", "USD": "US"}
	if method == "wechat_pay" {
		mapping["CHF"] = "CH"
		mapping["NOK"] = "NO"
		mapping["SEK"] = "SE"
		mapping["DKK"] = "DK"
	} else {
		if cur == "USD" {
			return slices.Contains(strings.Fields("AT BE BG CA HR CY CZ DK EE FI FR DE GI GR HK HU IE IT LV LI LT LU MT NL NO PT RO SG SK SI ES SE GB US"), country)
		}
		if cur == "GBP" && country == "GI" {
			return true
		}
		mapping["CHF"] = "CH"
		mapping["MYR"] = "MY"
		mapping["NZD"] = "NZ"
	}
	return mapping[cur] == country
}

func (a *Adapter) request(ctx context.Context, method, path string, form url.Values, key string, dst any) error {
	var in io.Reader
	if form != nil {
		in = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.stripe.com"+path, in)
	if err != nil {
		return err
	}
	// The durable outbox permits one POST. Disable transparent retry of a body-bearing request.
	req.GetBody = nil
	req.SetBasicAuth(a.cfg.TestKey, "")
	req.Header.Set("Stripe-Version", "2025-06-30.basil")
	if in != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("Stripe sandbox transport: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("Stripe sandbox returned HTTP %d; original outcome requires inquiry", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 || json.Unmarshal(data, dst) != nil {
		return ErrConflict
	}
	return nil
}

func (a *Adapter) scope(c railcontract.Command) error {
	if c.Merchant != a.cfg.Merchant || c.Account != a.cfg.GatewayAccount || c.ConnectorID != a.cfg.ConnectorID || c.ProviderAccount != a.cfg.AccountID || c.Channel != "hosted" || c.Terminal != "" || !slices.Contains(a.cfg.AllowedRoutes[c.RouteID], c.PaymentMethod) || c.PaymentMethod == "" || !slices.Contains(a.cfg.Capabilities[c.PaymentMethod], c.Currency) || c.Amount <= 0 || c.Amount > 99999999 || !tokenID.MatchString(c.ID) || !tokenID.MatchString(c.Order) || !tokenID.MatchString(c.Outlet) || !((c.Kind == "charge" && c.Parent == "") || (c.Kind == "refund" && tokenID.MatchString(c.Parent))) {
		return ErrConflict
	}
	return nil
}

func (a *Adapter) Identity(ctx context.Context, c railcontract.Command) (string, error) {
	if err := a.scope(c); err != nil {
		return "", err
	}
	var actual string
	if err := a.db.QueryRow(ctx, "SELECT ledger FROM partnerstripe.installation WHERE singleton").Scan(&actual); err != nil {
		return "", err
	}
	if expected := railcontract.ExpectedProcessorIdentity(ctx); actual != a.ledger || (expected != "" && expected != actual) {
		return "", railcontract.ErrProcessorIdentityMismatch
	}
	return actual, nil
}
func (a *Adapter) fence(ctx context.Context, c railcontract.Command) error {
	if railcontract.ExpectedProcessorIdentity(ctx) != a.ledger {
		return railcontract.ErrProcessorIdentityMismatch
	}
	_, err := a.Identity(ctx, c)
	return err
}

type stored struct {
	Command                          railcontract.Command
	Digest, ProviderID, ParentCharge string
}

func (a *Adapter) operation(ctx context.Context, id string) (stored, error) {
	var v stored
	var raw []byte
	err := a.db.QueryRow(ctx, `SELECT terms,digest,coalesce(provider_id,''),parent_charge FROM partnerstripe.operation WHERE ledger=$1 AND id=$2`, a.ledger, id).Scan(&raw, &v.Digest, &v.ProviderID, &v.ParentCharge)
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal(raw, &v.Command); err != nil {
		return v, err
	}
	return v, nil
}
func digest(c railcontract.Command) string {
	p, _ := json.Marshal(c)
	d := sha256.Sum256(p)
	return hex.EncodeToString(d[:])
}
func (a *Adapter) idempotency(c railcontract.Command) string {
	return "dakim-" + a.ledger + "-" + digest(c)
}

// Submit owns exactly one network submission. Losing the response permanently
// removes local permission to POST. Recover() can bind an original found at Stripe.
func (a *Adapter) Submit(ctx context.Context, c railcontract.Command) error {
	if err := a.fence(ctx, c); err != nil {
		return err
	}
	sc := stored{Command: c, Digest: digest(c)}
	if c.Kind == "refund" {
		p, err := a.operation(ctx, c.Parent)
		if err != nil {
			return err
		}
		expected := p.Command
		expected.ID = c.ID
		expected.Kind = c.Kind
		expected.Parent = c.Parent
		expected.Amount = c.Amount
		if p.Command.Kind != "charge" || expected != c || c.Amount > p.Command.Amount {
			return ErrConflict
		}
		ev, err := a.inquire(ctx, p, p.ProviderID)
		if err != nil {
			return err
		}
		if ev.Fact.State != "captured" {
			return ErrConflict
		}
		sc.ParentCharge = ev.Fact.Reference
	}
	raw, _ := json.Marshal(c)
	tag, err := a.db.Exec(ctx, `INSERT INTO partnerstripe.operation(id,ledger,terms,digest,parent_charge) SELECT $1,ledger,$3,$4,$5 FROM partnerstripe.installation WHERE ledger=$2 ON CONFLICT DO NOTHING`, c.ID, a.ledger, raw, sc.Digest, sc.ParentCharge)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		existing, err := a.operation(ctx, c.ID)
		if err != nil {
			return err
		}
		if existing.Command != c {
			return ErrConflict
		}
		if existing.ProviderID == "" {
			return ErrUnknown
		}
		return nil
	}
	form := url.Values{}
	path := "/v1/checkout/sessions"
	if c.Kind == "charge" {
		form.Set("mode", "payment")
		form.Set("success_url", a.cfg.ReturnURL)
		form.Set("cancel_url", a.cfg.ReturnURL)
		form.Set("client_reference_id", c.ID)
		form.Set("line_items[0][price_data][currency]", strings.ToLower(c.Currency))
		form.Set("line_items[0][price_data][unit_amount]", fmt.Sprint(c.Amount))
		form.Set("line_items[0][price_data][product_data][name]", "dakim pay sandbox order")
		form.Set("line_items[0][quantity]", "1")
		form.Set("payment_method_types[0]", c.PaymentMethod)
		form.Set("adaptive_pricing[enabled]", "false")
		if c.PaymentMethod == "wechat_pay" {
			form.Set("payment_method_options[wechat_pay][client]", "web")
		}
		a.metadata(form, "payment_intent_data[metadata]", sc)
	} else {
		path = "/v1/refunds"
		form.Set("charge", sc.ParentCharge)
		form.Set("amount", fmt.Sprint(c.Amount))
	}
	a.metadata(form, "metadata", sc)
	var ret struct {
		ID, Object string
		Livemode   *bool
	}
	if err = a.request(ctx, "POST", path, form, a.idempotency(c), &ret); err != nil {
		return err
	}
	if !validObject(c.Kind, ret.ID) || (c.Kind == "charge" && (ret.Object != "checkout.session" || ret.Livemode == nil || *ret.Livemode)) || (c.Kind == "refund" && ret.Object != "refund") {
		return ErrConflict
	}
	return a.bind(ctx, sc, ret.ID)
}
func (a *Adapter) metadata(v url.Values, prefix string, r stored) {
	v.Set(prefix+"[dakim_terms]", r.Digest)
	v.Set(prefix+"[dakim_ledger]", a.ledger)
	v.Set(prefix+"[dakim_operation]", r.Command.ID)
}
func validObject(kind, id string) bool {
	return stripeID.MatchString(id) && ((kind == "charge" && strings.HasPrefix(id, "cs_test_")) || (kind == "refund" && strings.HasPrefix(id, "re_")))
}
func (a *Adapter) bind(ctx context.Context, r stored, id string) error {
	tag, err := a.db.Exec(ctx, `UPDATE partnerstripe.operation SET provider_id=$3 WHERE id=$1 AND ledger=$2 AND digest=$4 AND (provider_id IS NULL OR provider_id=$3)`, r.Command.ID, a.ledger, id, r.Digest)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}
func (a *Adapter) Lookup(ctx context.Context, c railcontract.Command) (railcontract.Fact, bool, error) {
	if err := a.fence(ctx, c); err != nil {
		return railcontract.Fact{}, false, err
	}
	r, err := a.operation(ctx, c.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return railcontract.Fact{}, false, nil
	}
	if err != nil {
		return railcontract.Fact{}, false, err
	}
	if r.Command != c {
		return railcontract.Fact{}, false, ErrConflict
	}
	e, err := a.inquire(ctx, r, r.ProviderID)
	return e.Fact, err == nil, err
}

// Recover binds a provider ID only after a read of the exact original object and full evidence verification.
func (a *Adapter) Recover(ctx context.Context, operation, id string) (Evidence, error) {
	r, err := a.operation(ctx, operation)
	if err != nil {
		return Evidence{}, err
	}
	if r.ProviderID != "" && r.ProviderID != id {
		return Evidence{}, ErrConflict
	}
	if _, err = a.Identity(ctx, r.Command); err != nil {
		return Evidence{}, err
	}
	ev, err := a.inquire(ctx, r, id)
	if err != nil {
		return Evidence{}, err
	}
	return ev, a.bind(ctx, r, id)
}
func (a *Adapter) Read(ctx context.Context, operation string) (Evidence, error) {
	r, err := a.operation(ctx, operation)
	if err != nil {
		return Evidence{}, err
	}
	if _, err = a.Identity(ctx, r.Command); err != nil {
		return Evidence{}, err
	}
	return a.inquire(ctx, r, r.ProviderID)
}

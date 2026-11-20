package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"dakimpay/internal/aggregation"
	"dakimpay/internal/gateway"
	"dakimpay/internal/virtualrail"
)

type credentialConfig struct {
	Merchant          string            `json:"merchant"`
	Role              gateway.Role      `json:"role"`
	Subject           string            `json:"subject"`
	SHA256            string            `json:"sha256"`
	RouteID           string            `json:"route_id,omitempty"`
	ProviderAccount   string            `json:"provider_account,omitempty"`
	FeeCorrectionKeys map[string]string `json:"fee_correction_keys,omitempty"`
}
type programConfig struct {
	Merchant    string            `json:"merchant"`
	Account     string            `json:"account"`
	Currency    string            `json:"currency"`
	Beneficiary string            `json:"beneficiary"`
	MaxAmount   int64             `json:"max_amount"`
	Outlets     []string          `json:"outlets"`
	Terminals   map[string]string `json:"terminals"`
}
type connectorConfig struct {
	ID         string `json:"id"`
	Kind       string `json:"kind,omitempty"`
	Origin     string `json:"origin,omitempty"`
	Token      string `json:"token,omitempty"`
	ConfigFile string `json:"config_file,omitempty"`
}
type gatewayConfig struct {
	Connectors    []connectorConfig      `json:"connectors,omitempty"`
	Routes        []aggregation.Route    `json:"routes,omitempty"`
	Origin        string                 `json:"origin"`
	RailOrigin    string                 `json:"rail_origin"`
	RailToken     string                 `json:"rail_token"`
	Programs      []programConfig        `json:"programs"`
	Credentials   []credentialConfig     `json:"credentials"`
	Subscriptions []gateway.Subscription `json:"subscriptions"`
}
type railConfig struct {
	Origin    string `json:"origin"`
	RailToken string `json:"rail_token"`
	Programs  []struct {
		RouteID         string   `json:"route_id,omitempty"`
		ConnectorID     string   `json:"connector_id,omitempty"`
		ProviderAccount string   `json:"provider_account,omitempty"`
		PaymentMethods  []string `json:"payment_methods,omitempty"`
		Beneficiary     string   `json:"beneficiary"`
		StatementToken  string   `json:"statement_token"`
		Merchant        string   `json:"merchant"`
		Account         string   `json:"account"`
		Currency        string   `json:"currency"`
		MaxAmount       int64    `json:"max_amount"`
		Outlets         []string `json:"outlets"`
		Terminals       []struct {
			ID     string `json:"id"`
			Outlet string `json:"outlet"`
			Token  string `json:"token"`
		} `json:"terminals"`
	} `json:"programs"`
}

func readConfig(path string, out any) error {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		return err
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return errors.New("config must be a private regular file owned by the service account")
	}
	const max = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || len(raw) > max {
		return errors.New("unreadable or oversized configuration")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := checkConfigValue(d, reflect.TypeOf(out).Elem()); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected exactly one configuration object")
	}
	return json.Unmarshal(raw, out)
}

// The typed decoder alone accepts case-insensitive aliases and overwrites earlier
// values when keys repeat. Inspect the exact schema at every level first,
// preserving dynamic, case-sensitive terminal IDs in enrollment maps.
func checkConfigValue(d *json.Decoder, typ reflect.Type) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token == nil {
		if typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			return nil
		}
		return errors.New("null configuration value")
	}
	switch typ.Kind() {
	case reflect.Struct, reflect.Map:
		if token != json.Delim('{') {
			return errors.New("expected configuration object")
		}
		fields := map[string]reflect.Type{}
		if typ.Kind() == reflect.Struct {
			for i := range typ.NumField() {
				f := typ.Field(i)
				name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
				fields[name] = f.Type
			}
		}
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			name, ok := k.(string)
			if err != nil || !ok || seen[name] {
				return errors.New("duplicate or invalid configuration field")
			}
			seen[name] = true
			nested := fields[name]
			if typ.Kind() == reflect.Map {
				nested = typ.Elem()
			}
			if nested == nil {
				return errors.New("unknown configuration field")
			}
			if err := checkConfigValue(d, nested); err != nil {
				return err
			}
		}
		_, err = d.Token()
	case reflect.Slice:
		if token != json.Delim('[') {
			return errors.New("expected configuration array")
		}
		for d.More() {
			if err := checkConfigValue(d, typ.Elem()); err != nil {
				return err
			}
		}
		_, err = d.Token()
	default:
		if _, ok := token.(json.Delim); ok {
			return errors.New("expected scalar configuration field")
		}
	}
	return err
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,95}$`)
var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

func loopbackOrigin(origin string) (*url.URL, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return nil, errors.New("local qualification requires an explicit http://127.0.0.1:port origin")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || origin != "http://127.0.0.1:"+strconv.Itoa(port) {
		return nil, errors.New("invalid local port")
	}
	return u, nil
}
func emulatorEndpoint(s string) bool {
	host, port, err := net.SplitHostPort(s)
	ip := net.ParseIP(host)
	n, e := strconv.Atoi(port)
	return err == nil && e == nil && n > 0 && n <= 65535 && ip != nil && ip.IsLoopback()
}
func (c gatewayConfig) principals() (map[[32]byte]gateway.Principal, error) {
	if _, e := loopbackOrigin(c.Origin); e != nil {
		return nil, e
	}
	seenSecrets, err := c.validateConnectorEndpoints()
	if err != nil {
		return nil, err
	}
	programs := map[string]gateway.Principal{}
	accounts := map[string]bool{}
	for _, v := range c.Programs {
		if !safeID.MatchString(v.Merchant) || !safeID.MatchString(v.Account) || !safeID.MatchString(v.Beneficiary) || !currencyCode.MatchString(v.Currency) || v.MaxAmount < 1 || v.MaxAmount > 1_000_000_000_000 || len(v.Outlets) == 0 {
			return nil, errors.New("invalid program authority")
		}
		if _, ok := programs[v.Merchant]; ok || accounts[v.Account] {
			return nil, errors.New("duplicate merchant or virtual processor account")
		}
		accounts[v.Account] = true
		outlets := map[string]bool{}
		for _, o := range v.Outlets {
			if !safeID.MatchString(o) || outlets[o] {
				return nil, errors.New("invalid or duplicate outlet")
			}
			outlets[o] = true
		}
		for t, o := range v.Terminals {
			if !safeID.MatchString(t) || !outlets[o] {
				return nil, errors.New("invalid terminal assignment")
			}
		}
		programs[v.Merchant] = gateway.Principal{Merchant: v.Merchant, Account: v.Account, Currency: v.Currency, Beneficiary: v.Beneficiary, MaxAmount: v.MaxAmount, Outlets: outlets, Terminals: v.Terminals}
	}
	if len(programs) == 0 || len(c.Credentials) == 0 {
		return nil, errors.New("no merchant programs or API grants configured")
	}
	for _, route := range c.Routes {
		p, ok := programs[route.Merchant]
		if !ok || p.Account != route.Account || p.Currency != route.Currency || route.MaxAmount > p.MaxAmount {
			return nil, errors.New("route exceeds its enrolled merchant program")
		}
	}
	principals := map[[32]byte]gateway.Principal{}
	for _, v := range c.Credentials {
		p, ok := programs[v.Merchant]
		if !ok || !safeID.MatchString(v.Subject) {
			return nil, errors.New("credential has no valid program or subject")
		}
		switch v.Role {
		case gateway.MerchantRole, gateway.StatementRole, gateway.BankRole, gateway.FinanceRole, gateway.ReviewerRole:
		default:
			return nil, errors.New("unknown credential role")
		}
		if v.RouteID != "" || v.ProviderAccount != "" {
			valid := false
			for _, rt := range c.Routes {
				if rt.Merchant == p.Merchant && rt.Account == p.Account && rt.ID == v.RouteID && rt.ProviderAccount == v.ProviderAccount {
					valid = true
					break
				}
			}
			if v.Role != gateway.StatementRole || !valid {
				return nil, errors.New("invalid statement origin grant")
			}
			p.StatementRoute, p.StatementProviderAccount = v.RouteID, v.ProviderAccount
		}
		if len(v.FeeCorrectionKeys) != 0 {
			if v.Role != gateway.StatementRole || p.StatementRoute == "" || p.StatementProviderAccount == "" {
				return nil, errors.New("fee correction verification keys require a route-scoped statement grant")
			}
			for id, key := range v.FeeCorrectionKeys {
				raw, err := hex.DecodeString(key)
				if !safeID.MatchString(id) || err != nil || len(raw) != ed25519.PublicKeySize || hex.EncodeToString(raw) != key {
					return nil, errors.New("invalid fee correction verification key")
				}
			}
			p.FeeCorrectionKeys = v.FeeCorrectionKeys
		}
		raw, err := hex.DecodeString(v.SHA256)
		if err != nil || len(raw) != sha256.Size || strings.ToLower(v.SHA256) != v.SHA256 {
			return nil, errors.New("invalid bearer token digest")
		}
		digest := [32]byte(raw)
		if seenSecrets[digest] {
			return nil, errors.New("credential reused across authorities")
		}
		seenSecrets[digest] = true
		p.Role, p.Subject = v.Role, v.Subject
		principals[digest] = p
	}
	if err := gateway.ValidateSubscriptions(c.Subscriptions); err != nil {
		return nil, err
	}
	for _, sub := range c.Subscriptions {
		p, ok := programs[sub.Merchant]
		k := sha256.Sum256([]byte(sub.Secret))
		if !ok || p.Account != sub.Account || seenSecrets[k] {
			return nil, errors.New("invalid webhook program or reused signing secret")
		}
		seenSecrets[k] = true
	}
	return principals, nil
}
func (c railConfig) programs() ([]virtualrail.Program, error) {
	if len(c.RailToken) < 32 {
		return nil, errors.New("a strong rail service token is required")
	}
	if _, err := loopbackOrigin(c.Origin); err != nil {
		return nil, err
	}
	seen := map[[32]byte]bool{sha256.Sum256([]byte(c.RailToken)): true}
	programs := make([]virtualrail.Program, 0, len(c.Programs))
	for _, x := range c.Programs {
		if len(x.StatementToken) < 32 || !safeID.MatchString(x.Beneficiary) {
			return nil, errors.New("each program requires its own export credential and beneficiary alias")
		}
		hash := sha256.Sum256([]byte(x.StatementToken))
		if seen[hash] {
			return nil, errors.New("statement export credential reused")
		}
		seen[hash] = true
		p := virtualrail.Program{RouteID: x.RouteID, ConnectorID: x.ConnectorID, ProviderAccount: x.ProviderAccount, PaymentMethods: x.PaymentMethods, Beneficiary: x.Beneficiary, StatementToken: x.StatementToken, Merchant: x.Merchant, Account: x.Account, Currency: x.Currency, MaxAmount: x.MaxAmount, Outlets: x.Outlets}
		for _, t := range x.Terminals {
			k := sha256.Sum256([]byte(t.Token))
			if len(t.Token) < 32 || seen[k] {
				return nil, errors.New("each device requires a distinct high-entropy credential")
			}
			seen[k] = true
			p.Terminals = append(p.Terminals, virtualrail.Terminal{ID: t.ID, Outlet: t.Outlet, Token: t.Token})
		}
		programs = append(programs, p)
	}
	return programs, nil
}

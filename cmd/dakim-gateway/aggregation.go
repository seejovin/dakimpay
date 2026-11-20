// Connector secrets and settlement grants belong to distinct merchant authorities.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"

	"dakimpay/internal/aggregation"
	"dakimpay/internal/gateway"
	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
	"dakimpay/internal/virtualrail"
)

func (c gatewayConfig) validateConnectorEndpoints() (map[[32]byte]bool, error) {
	secrets := make(map[[32]byte]bool)
	ids, origins, privateFiles := make(map[string]bool), map[string]bool{c.Origin: true}, make(map[string]bool)
	if c.RailOrigin != "" || c.RailToken != "" || len(c.Connectors) == 0 {
		if _, err := loopbackOrigin(c.RailOrigin); err != nil || c.RailOrigin == c.Origin || len(c.RailToken) < 32 {
			return nil, errors.New("independent legacy rail origin and credential required")
		}
		secrets[sha256.Sum256([]byte(c.RailToken))] = true
	}
	if (len(c.Routes) == 0) != (len(c.Connectors) == 0) || len(c.Connectors) > 128 {
		return nil, errors.New("connector catalog and enrollments must be supplied together")
	}
	for _, connector := range c.Connectors {
		if !safeID.MatchString(connector.ID) || ids[connector.ID] {
			return nil, errors.New("invalid or duplicate connector identity")
		}
		ids[connector.ID] = true
		switch connector.Kind {
		case "", "virtual":
			if _, err := loopbackOrigin(connector.Origin); err != nil || origins[connector.Origin] || connector.ConfigFile != "" || len(connector.Token) < 32 {
				return nil, errors.New("each virtual connector requires an independent origin and credential")
			}
			k := sha256.Sum256([]byte(connector.Token))
			sameLegacy := connector.Origin == c.RailOrigin && connector.Token == c.RailToken
			if (secrets[k] || connector.Origin == c.RailOrigin) && !sameLegacy {
				return nil, errors.New("connector credential or origin reused across authorities")
			}
			secrets[k], origins[connector.Origin] = true, true
		case "stripe_test", "antom_test":
			if !filepath.IsAbs(connector.ConfigFile) || filepath.Clean(connector.ConfigFile) != connector.ConfigFile || privateFiles[connector.ConfigFile] || connector.Origin != "" || connector.Token != "" {
				return nil, errors.New("partner test connector requires its own absolute private configuration file")
			}
			privateFiles[connector.ConfigFile] = true
		default:
			return nil, errors.New("unsupported connector type")
		}
	}
	return secrets, nil
}

func (c gatewayConfig) openConnectors(ctx context.Context, store gwstore.Store) (railcontract.Processor, *aggregation.Router, func(), error) {
	var legacy railcontract.Processor
	closeAll := func() {}
	if c.RailOrigin != "" {
		client, err := virtualrail.NewClient(c.RailOrigin, c.RailToken)
		if err != nil {
			return nil, nil, nil, err
		}
		legacy = client
	}
	if len(c.Connectors) == 0 {
		return legacy, nil, closeAll, nil
	}
	clients := make(map[string]railcontract.Processor, len(c.Connectors))
	var closers []func()
	closeAll = func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	for _, cfg := range c.Connectors {
		var client railcontract.Processor
		var err error
		if cfg.Kind == "stripe_test" || cfg.Kind == "antom_test" {
			var done func()
			client, done, err = c.openPartner(ctx, cfg, store)
			if done != nil {
				closers = append(closers, done)
			}
		} else {
			client, err = virtualrail.NewClient(cfg.Origin, cfg.Token)
		}
		if err != nil {
			closeAll()
			return nil, nil, nil, fmt.Errorf("connector %s: %w", cfg.ID, err)
		}
		clients[cfg.ID] = client
	}
	router, err := aggregation.New(c.Routes, clients, legacy)
	if err != nil {
		closeAll()
		return nil, nil, nil, err
	}
	return router, router, closeAll, nil
}

func methodsFor(catalog *aggregation.Router, p gateway.Principal, channel string, amount int64) []string {
	if catalog == nil {
		return []string{"card"}
	}
	methods := catalog.Methods(p.Merchant, p.Account, p.Currency, channel, amount)
	if methods == nil {
		return []string{}
	}
	return methods
}
func paymentMethods(w http.ResponseWriter, r *http.Request, catalog *aggregation.Router, core *gateway.Service) {
	p, _ := gateway.PrincipalFrom(r.Context())
	if p.Role != gateway.MerchantRole {
		http.Error(w, "merchant credential required", 403)
		return
	}
	q := r.URL.Query()
	for k, v := range q {
		if len(v) != 1 || (k != "channel" && k != "amount_minor") {
			http.Error(w, "invalid query", 400)
			return
		}
	}
	channel := q.Get("channel")
	if channel != "hosted" && channel != "terminal" {
		http.Error(w, "channel must be hosted or terminal", 400)
		return
	}
	var amount int64
	if q.Has("amount_minor") {
		n, err := strconv.ParseInt(q.Get("amount_minor"), 10, 64)
		if err != nil || n < 1 || n > p.MaxAmount {
			http.Error(w, "invalid program amount", 400)
			return
		}
		amount = n
	}
	w.Header().Set("Content-Type", "application/json")
	list := methodsFor(catalog, p, channel, amount)
	if catalog != nil {
		var err error
		list, err = core.AvailableMethods(r.Context(), p, channel, amount, list)
		if err != nil {
			http.Error(w, "route availability unavailable", 503)
			return
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"currency": p.Currency, "channel": channel, "methods": list})
}

// Return only checkout origins that this authenticated program can use,
// including disabled routes that may still own a pending original.
func (c gatewayConfig) checkoutOrigins(p gateway.Principal) map[string]string {
	origins := make(map[string]string)
	if c.RailOrigin != "" {
		origins[""] = c.RailOrigin
	}
	providers := make(map[string]string)
	for _, connector := range c.Connectors {
		if connector.Kind == "antom_test" {
			providers[connector.ID] = "antom_test"
		} else if connector.Kind == "stripe_test" {
			providers[connector.ID] = "https://checkout.stripe.com"
		} else {
			providers[connector.ID] = connector.Origin
		}
	}
	for _, route := range c.Routes {
		if route.Merchant == p.Merchant && route.Account == p.Account {
			origins[route.Connector] = providers[route.Connector]
		}
	}
	return origins
}

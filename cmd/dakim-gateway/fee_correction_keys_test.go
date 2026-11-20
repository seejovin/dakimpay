package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"

	"dakimpay/internal/gateway"
)

func TestFeeCorrectionKeyEnrollment(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := hex.EncodeToString(pub)
	enrolled := func() gatewayConfig {
		cfg := oneCatalog()
		cfg.Credentials[len(cfg.Credentials)-1].FeeCorrectionKeys = map[string]string{"processor-01": raw}
		return cfg
	}
	cfg := enrolled()
	grants, err := cfg.principals()
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, p := range grants {
		if len(p.FeeCorrectionKeys) == 0 {
			continue
		}
		if p.Role != gateway.StatementRole || p.Subject != "east-importer" || p.Merchant != "one" ||
			p.StatementRoute != "acquiring" || p.StatementProviderAccount != "east-one" ||
			p.FeeCorrectionKeys["processor-01"] != raw {
			t.Fatal("verification key attached to a different grant")
		}
		count++
	}
	if count != 1 {
		t.Fatalf("expected exactly one enrolled issuer, got %d", count)
	}

	for name, mutate := range map[string]func(*credentialConfig){
		"bank role": func(c *credentialConfig) {
			c.Role, c.RouteID, c.ProviderAccount = gateway.BankRole, "", ""
		},
		"unscoped statement":      func(c *credentialConfig) { c.RouteID, c.ProviderAccount = "", "" },
		"other processor account": func(c *credentialConfig) { c.ProviderAccount = "wallet-one" },
		"non-canonical hex":       func(c *credentialConfig) { c.FeeCorrectionKeys["processor-01"] = strings.Repeat("AA", 32) },
		"wrong key length":        func(c *credentialConfig) { c.FeeCorrectionKeys["processor-01"] = raw[:62] },
		"invalid hex":             func(c *credentialConfig) { c.FeeCorrectionKeys["processor-01"] = strings.Repeat("g", 64) },
		"invalid id":              func(c *credentialConfig) { c.FeeCorrectionKeys["bad/id"] = raw },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := enrolled()
			mutate(&cfg.Credentials[len(cfg.Credentials)-1])
			if _, err := cfg.principals(); err == nil {
				t.Fatal("invalid issuer enrollment accepted")
			}
		})
	}
}

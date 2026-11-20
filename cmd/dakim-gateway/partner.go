package main

import (
	"context"
	"errors"
	"slices"

	"dakimpay/internal/antomtest"
	"dakimpay/internal/gwstore"
	"dakimpay/internal/partnerstripe"
	"dakimpay/internal/railcontract"
)

// API traffic is pinned to the provider test endpoint by the adapter.
// Credentials and connector records have their own storage and authority.
func (c gatewayConfig) openPartner(ctx context.Context, source connectorConfig, merchantStore gwstore.Store) (railcontract.Processor, func(), error) {
	if source.Kind == "antom_test" {
		return c.openAntom(ctx, source, merchantStore)
	}
	var f struct {
		DatabaseURL string               `json:"database_url"`
		Settings    partnerstripe.Config `json:"settings"`
	}
	if err := readConfig(source.ConfigFile, &f); err != nil {
		return nil, nil, err
	}
	cfg := f.Settings
	if cfg.ConnectorID != source.ID || f.DatabaseURL == "" {
		return nil, nil, errors.New("partner account and connector enrollment do not match")
	}
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	var owner nativeOwner
	count := 0
	for _, r := range c.Routes {
		if r.Connector != source.ID {
			continue
		}
		count++
		owner = nativeOwner{r.Merchant, r.Account, r.Connector, r.ID}
		methods, ok := cfg.AllowedRoutes[r.ID]
		if !ok || r.Merchant != cfg.Merchant || r.Account != cfg.GatewayAccount || r.ProviderAccount != cfg.AccountID ||
			len(r.Methods) != len(methods) || len(r.Channels) != 1 || r.Channels[0] != "hosted" {
			return nil, nil, errors.New("partner enrollment differs from gateway route")
		}
		for _, m := range r.Methods {
			if !slices.Contains(methods, m) || !slices.Contains(cfg.Capabilities[m], r.Currency) {
				return nil, nil, errors.New("gateway method or currency missing from partner enrollment")
			}
		}
	}
	if count != 1 || count != len(cfg.AllowedRoutes) {
		return nil, nil, errors.New("partner route list differs from gateway catalog")
	}
	pool, err := localPostgres(ctx, f.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	if err := gwstore.InstallPostgres(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, err
	}
	side, err := gwstore.NewPostgres(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	if err = claimStore(ctx, side, "stripe-test/"+source.ID); err != nil {
		pool.Close()
		return nil, nil, err
	}
	if err = claimPartner(ctx, merchantStore, "stripe_test", []string{cfg.AccountID}, owner); err != nil {
		pool.Close()
		return nil, nil, err
	}
	lazy := &lazyPartner{open: func(ctx context.Context) (fullProcessor, error) { return partnerstripe.New(ctx, pool, cfg) }}
	return lazy, pool.Close, nil
}

func (c gatewayConfig) openAntom(ctx context.Context, source connectorConfig, merchantStore gwstore.Store) (railcontract.Processor, func(), error) {
	var file struct {
		DatabaseURL string           `json:"database_url"`
		Settings    antomtest.Config `json:"settings"`
	}
	if err := readConfig(source.ConfigFile, &file); err != nil {
		return nil, nil, err
	}
	if file.DatabaseURL == "" {
		return nil, nil, errors.New("private Antom test ledger database required")
	}
	var owner nativeOwner
	enrollments := 0
	for _, r := range c.Routes {
		if r.Connector != source.ID {
			continue
		}
		enrollments++
		owner = nativeOwner{r.Merchant, r.Account, r.Connector, r.ID}
		if len(r.Channels) != 1 || r.Channels[0] != "hosted" || len(r.Methods) != 1 || r.Methods[0] != "alipay" || r.ProviderAccount != file.Settings.MerchantAccountID || r.Merchant != file.Settings.Merchant || r.Account != file.Settings.GatewayAccount || r.Connector != file.Settings.ConnectorID || r.ID != file.Settings.RouteID || r.Currency != file.Settings.Currency {
			return nil, nil, errors.New("Antom sandbox requires an enrolled hosted Alipay account")
		}
	}
	if enrollments != 1 {
		return nil, nil, errors.New("bind one Antom sandbox merchant enrollment per credential")
	}
	pool, err := localPostgres(ctx, file.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	if err = gwstore.InstallPostgres(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, err
	}
	store, err := gwstore.NewPostgres(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	if err = claimStore(ctx, store, "antom-test/"+source.ID); err != nil {
		pool.Close()
		return nil, nil, err
	}
	client, err := antomtest.New(file.Settings, store)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	if err = claimPartner(ctx, merchantStore, "antom_test", []string{file.Settings.MerchantAccountID, file.Settings.ClientID}, owner); err != nil {
		pool.Close()
		return nil, nil, err
	}
	return client, pool.Close, nil
}

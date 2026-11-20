package gateway

import (
	"context"
	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
	"errors"
)

// A selector may change only the route and resolve an omitted payment method.
// It cannot change the merchant's original amount, channel, order, or device.
func (s *Service) candidateRoutes(c railcontract.Command) ([]railcontract.Command, error) {
	if s.RouteCandidates != nil {
		out := s.RouteCandidates(c)
		if len(out) == 0 {
			return nil, engineFail(422, "payment_method_unavailable")
		}
		return out, nil
	}
	if s.SelectRoute == nil {
		if c.PaymentMethod != "" {
			return nil, engineFail(422, "payment_method_unavailable")
		}
		return []railcontract.Command{c}, nil
	}
	out, err := s.SelectRoute(c)
	if err != nil {
		return nil, engineFail(422, "payment_method_unavailable")
	}
	return []railcontract.Command{out}, nil
}

func chooseOpenRoute(tx gwstore.Tx, c railcontract.Command, candidates []railcontract.Command, legacy bool) (railcontract.Command, error) {
	for _, bound := range candidates {
		if legacy && bound == c && bound.PaymentMethod == "" {
			return c, nil
		}
		if !engineIdentifier.MatchString(bound.RouteID) || !engineIdentifier.MatchString(bound.ConnectorID) ||
			!engineIdentifier.MatchString(bound.ProviderAccount) || !engineIdentifier.MatchString(bound.PaymentMethod) ||
			(c.PaymentMethod != "" && c.PaymentMethod != bound.PaymentMethod) {
			return railcontract.Command{}, engineFail(503, "invalid_processor_route")
		}
		expected := c
		expected.PaymentMethod, expected.RouteID, expected.ConnectorID, expected.ProviderAccount = bound.PaymentMethod, bound.RouteID, bound.ConnectorID, bound.ProviderAccount
		if expected != bound {
			return railcontract.Command{}, engineFail(503, "invalid_processor_route")
		}
		control, err := readControl(tx, controlNamespace(c.Merchant, c.Account, bound.RouteID))
		if err != nil {
			return railcontract.Command{}, err
		}
		if !control.Suspended {
			return bound, nil
		}
	}
	return railcontract.Command{}, engineFail(409, "payment_routes_suspended")
}

type engineRouteEnrollment struct {
	Ledger          string `json:"processor_ledger"`
	ConnectorID     string `json:"connector_id"`
	ProviderAccount string `json:"provider_account"`
}

func engineRouteKey(p Principal, routeID string) string {
	return engineKey("route-enrollment", p.Merchant, p.Account, routeID)
}

func engineRouteEnrolled(tx gwstore.Tx, p Principal, routeID, providerAccount string) (bool, error) {
	var route engineRouteEnrollment
	found, err := gwstore.GetJSON(tx, engineRouteKey(p, routeID), &route)
	return found && route.ProviderAccount == providerAccount && engineLedgerID.MatchString(route.Ledger), err
}

// A route ID may never be reused for a different provider account or connector.
// Preventing alternate IDs for the same provider account also protects the
// external movement namespace from being opened twice inside a merchant book.
func enrollProcessorRoute(tx gwstore.Tx, p Principal, c railcontract.Command, observed, original string) error {
	key := engineRouteKey(p, c.RouteID)
	var route engineRouteEnrollment
	found, err := gwstore.GetJSON(tx, key, &route)
	if err != nil {
		return err
	}
	if found && (route.ConnectorID != c.ConnectorID || route.ProviderAccount != c.ProviderAccount || !engineLedgerID.MatchString(route.Ledger)) {
		return engineFail(409, "original_processor_route_changed")
	}
	if original != "" && (!found || route.Ledger != original) {
		return engineFail(409, "processor_ledger_changed")
	}
	if observed == "" {
		return engineIdentityRequest{c, route.Ledger}
	}
	// This direct-settlement model grants each provider-native account to one
	// merchant program. Different connector aliases must not defeat tenant isolation.
	globalKey := engineKey("ledger-account-program", observed, c.ProviderAccount)
	ownerID := engineKey(p.Merchant, p.Account, c.RouteID)
	var programOwner string
	alreadyOwned, err := gwstore.GetJSON(tx, globalKey, &programOwner)
	if err != nil {
		return err
	}
	if alreadyOwned && programOwner != ownerID {
		return engineFail(409, "processor_account_already_enrolled")
	}
	if !alreadyOwned {
		if err := gwstore.PutJSON(tx, globalKey, ownerID); err != nil {
			return err
		}
	}
	if found {
		if route.Ledger != observed {
			return engineFail(409, "processor_ledger_changed")
		}
		return nil
	}
	// The observed ledger identity also fences connector aliases pointing
	// at the same economic account under a second deployment name.
	accountOwners := []string{
		engineKey("route-account-owner", p.Merchant, p.Account, c.ConnectorID, c.ProviderAccount),
		engineKey("ledger-account-owner", p.Merchant, p.Account, observed, c.ProviderAccount),
	}
	for _, ownerKey := range accountOwners {
		var owner string
		exists, err := gwstore.GetJSON(tx, ownerKey, &owner)
		if err != nil {
			return err
		}
		if exists && owner != c.RouteID {
			return engineFail(409, "processor_account_already_enrolled")
		}
		if err := gwstore.PutJSON(tx, ownerKey, c.RouteID); err != nil {
			return err
		}
	}
	return gwstore.PutJSON(tx, key, engineRouteEnrollment{Ledger: observed, ConnectorID: c.ConnectorID, ProviderAccount: c.ProviderAccount})
}

func matchesProcessorRoute(a, b railcontract.Command) bool {
	return a.RouteID == b.RouteID && a.ConnectorID == b.ConnectorID && a.ProviderAccount == b.ProviderAccount && a.PaymentMethod == b.PaymentMethod
}

// Captured money remains a financial fact even if the acquiring source reports
// a different brand. Explicit scheme selection requires positive brand evidence
// for merchant fulfilment. A blank brand is never inferred from the order.
func brandEvidenceIssue(f railcontract.Fact) string {
	if f.State != "captured" && f.State != "refunded" {
		return ""
	}
	switch f.PaymentMethod {
	case "visa", "mastercard", "amex":
		if f.CardBrand == "" {
			return "card_brand_unverified"
		}
		if f.CardBrand != f.PaymentMethod {
			return "card_brand_mismatch"
		}
	case "card":
		if f.CardBrand == "" {
			return "card_brand_unverified"
		}
	case "wechat_pay", "alipay":
		if f.CardBrand != "" {
			return "unexpected_card_brand"
		}
	}
	return ""
}

// AvailableMethods applies both kinds of suspension in one consistent snapshot
// for a concrete amount. A zero amount asks for the provisioned method catalog.
// Admission repeats the checks against the immutable commercial terms.
func (s *Service) AvailableMethods(ctx context.Context, p Principal, channel string, amount int64, catalogMethods []string) ([]string, error) {
	if amount == 0 {
		return append([]string{}, catalogMethods...), nil
	}
	out := []string{}
	err := s.Store.View(ctx, func(tx gwstore.Tx) error {
		out = out[:0]
		program, err := readControl(tx, p.Merchant)
		if err != nil || program.Suspended {
			return err
		}
		for _, method := range catalogMethods {
			command := railcontract.Command{Merchant: p.Merchant, Account: p.Account, Currency: p.Currency, Channel: channel, Amount: amount, PaymentMethod: method, Kind: "charge"}
			candidates, err := s.candidateRoutes(command)
			if err != nil {
				return err
			}
			_, err = chooseOpenRoute(tx, command, candidates, false)
			var blocked engineError
			if errors.As(err, &blocked) && blocked.code == "payment_routes_suspended" {
				continue
			}
			if err != nil {
				return err
			}
			out = append(out, method)
		}
		return nil
	})
	return out, err
}

package virtualrail

import (
	"context"
	"errors"

	"dakimpay/internal/railcontract"
	"github.com/jackc/pgx/v5"
)

func cardBrand(method string) bool {
	return method == "visa" || method == "mastercard" || method == "amex"
}
func wallet(method string) bool { return method == "wechat_pay" || method == "alipay" }

func checkMethods(p Program) (map[string]bool, error) {
	if p.RouteID == "" && p.ConnectorID == "" && p.ProviderAccount == "" && len(p.PaymentMethods) == 0 {
		return nil, nil
	}
	if !validID.MatchString(p.RouteID) || !validID.MatchString(p.ConnectorID) || !validID.MatchString(p.ProviderAccount) || len(p.PaymentMethods) == 0 {
		return nil, errors.New("incomplete virtual rail enrollment")
	}
	methods := map[string]bool{}
	for _, v := range p.PaymentMethods {
		if methods[v] || (v != "card" && !cardBrand(v) && !wallet(v)) {
			return nil, errors.New("unsupported or duplicate payment method")
		}
		methods[v] = true
	}
	return methods, nil
}

func bindRoute(ctx context.Context, tx pgx.Tx, p Program) error {
	if p.RouteID != "" {
		var legacy bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM virtualrail.operation WHERE merchant=$1 AND account=$2 AND route_id='' LIMIT 1)", p.Merchant, p.Account).Scan(&legacy); err != nil {
			return err
		}
		if legacy {
			return errors.New("route assignment requires a separate ledger for this existing legacy program")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO virtualrail.route_binding(merchant,account,route_id,connector_id,provider_account)
			VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, p.Merchant, p.Account, p.RouteID, p.ConnectorID, p.ProviderAccount); err != nil {
			return err
		}
	}
	var r, c, a string
	err := tx.QueryRow(ctx, "SELECT route_id,connector_id,provider_account FROM virtualrail.route_binding WHERE merchant=$1 AND account=$2", p.Merchant, p.Account).Scan(&r, &c, &a)
	if errors.Is(err, pgx.ErrNoRows) && p.RouteID == "" {
		return nil
	}
	if err != nil {
		return err
	}
	if r != p.RouteID || c != p.ConnectorID || a != p.ProviderAccount {
		return errors.New("virtual processor enrollment cannot silently reassign an existing route")
	}
	return nil
}

func sameRoute(a, b railcontract.Command) bool {
	return a.RouteID == b.RouteID && a.ConnectorID == b.ConnectorID && a.ProviderAccount == b.ProviderAccount && a.PaymentMethod == b.PaymentMethod
}

func acceptsRoute(p program, c railcontract.Command) bool {
	if c.RouteID != p.RouteID || c.ConnectorID != p.ConnectorID || c.ProviderAccount != p.ProviderAccount {
		return false
	}
	if p.RouteID == "" {
		return c.PaymentMethod == ""
	}
	if c.Kind == "refund" {
		return (c.PaymentMethod == "card" || cardBrand(c.PaymentMethod) || wallet(c.PaymentMethod)) && (!wallet(c.PaymentMethod) || c.Channel == "hosted")
	}
	return p.Methods[c.PaymentMethod] && (!wallet(c.PaymentMethod) || c.Channel == "hosted")
}

// The virtual instrument is chosen at the processor. An explicit instrument
// incompatible with the requested network must never produce a capture.
func selectedBrand(method, selected string) (string, error) {
	if method == "" || wallet(method) {
		if selected != "" {
			return "", reject(422, "This presentation does not accept a card selection.")
		}
		return "", nil
	}
	if selected == "" {
		selected = method
		if method == "card" {
			selected = "visa"
		}
	}
	if !cardBrand(selected) || (method != "card" && method != selected) {
		return "", reject(422, "The selected test card is outside the requested network.")
	}
	return selected, nil
}

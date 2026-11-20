package aggregation

import "dakimpay/internal/railcontract"

// RouteCandidates contains only static policy matches, in configured priority
// order. The gateway checks the durable incident controls before admission.
func (r *Router) RouteCandidates(c railcontract.Command) []railcontract.Command {
	if c.Kind != "charge" || c.Parent != "" || c.Amount <= 0 || c.RouteID != "" || c.ConnectorID != "" || c.ProviderAccount != "" {
		return nil
	}
	if c.PaymentMethod == "" {
		c.PaymentMethod = "card"
	}
	var out []railcontract.Command
	for _, route := range r.programs[program{c.Merchant, c.Account}] {
		if route.offers(c.Currency, c.Channel, c.PaymentMethod, c.Amount) {
			chosen := c
			chosen.RouteID, chosen.ConnectorID, chosen.ProviderAccount = route.ID, route.Connector, route.ProviderAccount
			out = append(out, chosen)
		}
	}
	return out
}
func (r *Router) RouteExists(merchant, account, routeID string) bool {
	_, ok := r.byID[program{merchant, account}][routeID]
	return ok
}

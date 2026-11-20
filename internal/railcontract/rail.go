// Package railcontract defines the authenticated processor interface. Device
// presentation is a separate protocol; only a verified processor lookup is
// allowed to create a financial posting in the gateway.
package railcontract

import "context"

type Command struct {
	// These fields identify the original acquiring route. Account stays the
	// gateway program account. Unrouted historical commands keep empty fields.
	PaymentMethod   string `json:"payment_method,omitempty"`
	RouteID         string `json:"route_id,omitempty"`
	ConnectorID     string `json:"connector_id,omitempty"`
	ProviderAccount string `json:"provider_account,omitempty"`
	ID              string `json:"operation_id"`
	Merchant        string `json:"merchant_id"`
	Account         string `json:"processor_account"`
	Kind            string `json:"kind"`                // charge or refund
	Parent          string `json:"parent_id,omitempty"` // original charge operation for a refund
	Order           string `json:"order_id"`
	Outlet          string `json:"outlet_id"`
	Terminal        string `json:"terminal_id,omitempty"`
	Channel         string `json:"channel"` // terminal or hosted
	Amount          int64  `json:"amount_minor"`
	Currency        string `json:"currency"`
}

// Fact echoes the immutable terms and a stable processor movement reference
// for a financial outcome. CheckoutURL is present only for a pending hosted order.
type Fact struct {
	Command
	CardBrand   string `json:"card_brand,omitempty"` // observed on the processor or card reader
	State       string `json:"state"`                // pending, captured, refunded or declined
	Reference   string `json:"reference,omitempty"`
	CheckoutURL string `json:"checkout_url,omitempty"`
}

type Processor interface {
	// Submit accepts the original idempotent command. The result is always queried.
	Submit(context.Context, Command) error
	// Lookup returns found=false only for an authoritative, scope-bound absence.
	Lookup(context.Context, Command) (fact Fact, found bool, err error)
}

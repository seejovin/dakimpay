package partnerstripe

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"dakimpay/internal/railcontract"
)

type Evidence struct {
	unreversed     bool
	Fact           railcontract.Fact `json:"operation"`
	ProviderObject string            `json:"provider_object"`
	CardNetwork    string            `json:"observed_card_network,omitempty"`
}
type charge struct {
	AmountRefunded               *int64 `json:"amount_refunded"`
	Refunded, Disputed           *bool
	ID, Object, Currency, Status string
	Amount                       int64
	AmountCaptured               int64 `json:"amount_captured"`
	Livemode                     *bool
	Paid, Captured               bool
	PaymentIntent                string `json:"payment_intent"`
	Details                      struct {
		Type string
		Card struct{ Brand, Network string }
	} `json:"payment_method_details"`
}
type intent struct {
	ID, Object, Status, Currency string
	Amount                       int64
	AmountReceived               int64 `json:"amount_received"`
	Livemode                     *bool
	Metadata                     map[string]string
	Latest                       *charge `json:"latest_charge"`
}
type session struct {
	ID, Object, Mode, Currency, Status, URL string
	Livemode                                *bool
	Total                                   int64    `json:"amount_total"`
	Client                                  string   `json:"client_reference_id"`
	Methods                                 []string `json:"payment_method_types"`
	PaymentStatus                           string   `json:"payment_status"`
	Intent                                  *intent  `json:"payment_intent"`
	Metadata                                map[string]string
}
type refund struct {
	ID, Object, Charge, Status, Currency string
	Amount                               int64
	Metadata                             map[string]string
}

func (a *Adapter) matched(m map[string]string, r stored) bool {
	return m["dakim_ledger"] == a.ledger && m["dakim_terms"] == r.Digest && m["dakim_operation"] == r.Command.ID
}

func (a *Adapter) inquire(ctx context.Context, r stored, id string) (Evidence, error) {
	if id == "" {
		return Evidence{}, ErrUnknown
	}
	if !validObject(r.Command.Kind, id) {
		return Evidence{}, ErrConflict
	}
	if r.Command.Kind == "refund" {
		return a.refundEvidence(ctx, r, id)
	}
	var s session
	if err := a.request(ctx, "GET", "/v1/checkout/sessions/"+id+"?expand%5B%5D=payment_intent.latest_charge", nil, "", &s); err != nil {
		return Evidence{}, err
	}
	c := r.Command
	if s.ID != id || s.Object != "checkout.session" || s.Mode != "payment" || s.Livemode == nil || *s.Livemode || s.Client != c.ID || s.Total != c.Amount || s.Currency != strings.ToLower(c.Currency) || !a.matched(s.Metadata, r) || len(s.Methods) != 1 || s.Methods[0] != c.PaymentMethod {
		return Evidence{}, ErrConflict
	}
	out := Evidence{Fact: railcontract.Fact{Command: c}, ProviderObject: id}
	pi := s.Intent
	if pi != nil && (pi.Object != "payment_intent" || !stripeID.MatchString(pi.ID) || !strings.HasPrefix(pi.ID, "pi_") || pi.Livemode == nil || *pi.Livemode || pi.Amount != c.Amount || pi.Currency != s.Currency || !a.matched(pi.Metadata, r)) {
		return Evidence{}, ErrConflict
	}
	switch {
	case s.Status == "open" && s.PaymentStatus == "unpaid":
		u, err := url.Parse(s.URL)
		if err != nil || len(s.URL) > 8192 || u.Scheme != "https" || u.Host != "checkout.stripe.com" || u.User != nil || u.Path != "/c/pay/"+id {
			return Evidence{}, ErrConflict
		}
		out.Fact.State = "pending"
		out.Fact.CheckoutURL = s.URL
	case s.Status == "expired" && s.PaymentStatus == "unpaid" && (pi == nil || pi.Status == "canceled"):
		out.Fact.State = "declined"
	case s.Status == "complete" && s.PaymentStatus == "paid":
		if pi == nil || pi.Status != "succeeded" || pi.AmountReceived != c.Amount || pi.Latest == nil {
			return Evidence{}, ErrConflict
		}
		ch := pi.Latest
		if ch.Object != "charge" || !stripeID.MatchString(ch.ID) || !strings.HasPrefix(ch.ID, "ch_") || ch.Livemode == nil || *ch.Livemode || !ch.Paid || !ch.Captured || ch.Status != "succeeded" || ch.Amount != c.Amount || ch.AmountCaptured != c.Amount || ch.Currency != s.Currency || ch.PaymentIntent != pi.ID || ch.Details.Type != c.PaymentMethod {
			return Evidence{}, ErrConflict
		}
		if c.PaymentMethod == "card" {
			if !tokenID.MatchString(ch.Details.Card.Brand) || (ch.Details.Card.Network != "" && !tokenID.MatchString(ch.Details.Card.Network)) {
				return Evidence{}, ErrConflict
			}
			out.Fact.CardBrand = ch.Details.Card.Brand
			out.CardNetwork = ch.Details.Card.Network
		}
		out.unreversed = ch.AmountRefunded != nil && *ch.AmountRefunded == 0 && ch.Refunded != nil && !*ch.Refunded && ch.Disputed != nil && !*ch.Disputed
		out.Fact.State = "captured"
		out.Fact.Reference = ch.ID
	default:
		// Expiration, a redirect or delayed payment confirmation never proves decline or capture on its own.
		return Evidence{}, ErrUnknown
	}
	return out, nil
}

func (a *Adapter) refundEvidence(ctx context.Context, r stored, id string) (Evidence, error) {
	var f refund
	if err := a.request(ctx, "GET", "/v1/refunds/"+id, nil, "", &f); err != nil {
		return Evidence{}, err
	}
	c := r.Command
	if f.ID != id || f.Object != "refund" || f.Amount != c.Amount || f.Currency != strings.ToLower(c.Currency) || f.Charge != r.ParentCharge || !a.matched(f.Metadata, r) {
		return Evidence{}, ErrConflict
	}
	parent, err := a.operation(ctx, c.Parent)
	if err != nil {
		return Evidence{}, err
	}
	if parent.Command.Kind != "charge" {
		return Evidence{}, ErrConflict
	}
	original, err := a.inquire(ctx, parent, parent.ProviderID)
	if err != nil {
		return Evidence{}, err
	}
	if original.Fact.State != "captured" || original.Fact.Reference != f.Charge {
		return Evidence{}, ErrConflict
	}
	out := Evidence{Fact: railcontract.Fact{Command: c}, ProviderObject: id, CardNetwork: original.CardNetwork}
	switch f.Status {
	case "succeeded":
		out.Fact.State = "refunded"
		out.Fact.Reference = id
		out.Fact.CardBrand = original.Fact.CardBrand
	case "failed", "canceled":
		out.Fact.State = "declined"
	case "pending", "requires_action":
		out.Fact.State = "pending"
	default:
		return Evidence{}, ErrUnknown
	}
	return out, nil
}

// An empty list is affirmative evidence only with a complete Stripe list envelope
// for the requested collection. A list with any item (including a pending refund)
// causes a hold. Never infer absence from a failed call or malformed envelope.
func (a *Adapter) emptyCollection(ctx context.Context, path, chargeID string) (bool, error) {
	var list struct {
		Object, URL string
		More        *bool `json:"has_more"`
		Data        []json.RawMessage
	}
	q := url.Values{"charge": {chargeID}, "limit": {"1"}}
	if err := a.request(ctx, "GET", path+"?"+q.Encode(), nil, "", &list); err != nil {
		return false, err
	}
	if list.Object != "list" || list.URL != path || list.More == nil || list.Data == nil {
		return false, ErrConflict
	}
	return !*list.More && len(list.Data) == 0, nil
}

// InspectRelease reports a bounded read interval. This connector can prove
// only the empty-reversal case. A single returned object is enough to hold
// first fulfilment, including an externally initiated pending refund.
func (a *Adapter) InspectRelease(ctx context.Context, c railcontract.Command) (railcontract.ReleaseInventory, error) {
	observed := time.Now().UTC()
	result := railcontract.ReleaseInventory{ObservedAt: observed, DisputeStatus: "unknown"}
	if err := a.fence(ctx, c); err != nil {
		return result, err
	}
	if c.Kind != "charge" {
		return result, ErrConflict
	}
	r, err := a.operation(ctx, c.ID)
	if err != nil {
		return result, err
	}
	if r.Command != c {
		return result, ErrConflict
	}
	before, err := a.inquire(ctx, r, r.ProviderID)
	if err != nil {
		return result, err
	}
	result.Fact = before.Fact
	if before.Fact.State != "captured" || !before.unreversed {
		return result, nil
	}
	for _, path := range []string{"/v1/refunds", "/v1/disputes"} {
		empty, err := a.emptyCollection(ctx, path, before.Fact.Reference)
		if err != nil {
			return result, err
		}
		if !empty {
			return result, nil
		}
	}
	after, err := a.inquire(ctx, r, r.ProviderID)
	if err != nil {
		return result, err
	}
	if !after.unreversed || after != before {
		return result, ErrConflict
	}
	if err := a.fence(ctx, c); err != nil {
		return result, err
	}
	result.Complete = true
	result.DisputeStatus = "none"
	result.Refunds = []railcontract.Fact{}
	return result, nil
}

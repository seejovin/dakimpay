package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
)

const maxMoney int64 = 9_000_000_000_000_000

var engineIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
var engineCurrency = regexp.MustCompile(`^[A-Z]{3}$`)

// The stored instruction is the sole dispatch source. Neither retries nor
// local setup changes can choose a different amount, rail account or terminal.
type engineOperation struct {
	CardBrand          string               `json:"card_brand,omitempty"`
	Command            railcontract.Command `json:"instruction"`
	Ledger             string               `json:"processor_ledger"`
	Allocation         string               `json:"allocation_id"`
	Quote              string               `json:"quote_version"`
	RefundKey          string               `json:"merchant_refund_id,omitempty"`
	Status             string               `json:"status"`
	Version            int64                `json:"version"`
	Reference          string               `json:"reference,omitempty"`
	CheckoutURL        string               `json:"checkout_url,omitempty"`
	Refunded           int64                `json:"refunded_minor"`
	Reserved           int64                `json:"reserved_refund_minor"`
	Reason             string               `json:"reason,omitempty"`
	InquiryOnly        bool                 `json:"inquiry_only,omitempty"`
	EgressCommitted    bool                 `json:"egress_committed,omitempty"`
	FinancialReference string               `json:"held_financial_reference,omitempty"`
	Ticket             string               `json:"ticket,omitempty"`
	Attempts           int                  `json:"attempts"`
	Created            time.Time            `json:"created_at"`
	Updated            time.Time            `json:"updated_at"`
}

type engineIntentInput struct {
	PaymentMethod string `json:"payment_method,omitempty"`
	Order         string `json:"order_id"`
	Allocation    string `json:"allocation_id"`
	Outlet        string `json:"outlet_id"`
	Terminal      string `json:"terminal_id,omitempty"`
	Channel       string `json:"channel"`
	Amount        int64  `json:"amount_minor"`
	Currency      string `json:"currency"`
	Quote         string `json:"quote_version,omitempty"`
	OrderTotal    int64  `json:"order_total_minor,omitempty"`
	Replaces      string `json:"replaces_id,omitempty"`
}
type engineRefundInput struct {
	Amount     int64  `json:"amount_minor"`
	BusinessID string `json:"merchant_refund_id"`
}
type engineOrder struct {
	Total    int64  `json:"total"`
	Reserved int64  `json:"reserved"`
	Captured int64  `json:"captured"`
	Quote    string `json:"quote"`
	Currency string `json:"currency"`
	Outlet   string `json:"outlet"`
	Account  string `json:"account"`
}
type engineBinding struct{ ID, Digest string }
type engineWork struct{ Merchant, ID string }
type engineEvent struct {
	PaymentMethod   string    `json:"payment_method,omitempty"`
	RouteID         string    `json:"route_id,omitempty"`
	ConnectorID     string    `json:"connector_id,omitempty"`
	ProviderAccount string    `json:"provider_account,omitempty"`
	CardBrand       string    `json:"card_brand,omitempty"`
	Reference       string    `json:"reference,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	Refunded        int64     `json:"refunded_minor"`
	Reserved        int64     `json:"reserved_refund_minor"`
	Sequence        int64     `json:"sequence"`
	Operation       string    `json:"operation_id"`
	Parent          string    `json:"parent_id,omitempty"`
	Outlet          string    `json:"outlet_id"`
	Account         string    `json:"processor_account"`
	Kind            string    `json:"kind"`
	Status          string    `json:"status"`
	Version         int64     `json:"version"`
	Amount          int64     `json:"amount_minor"`
	Currency        string    `json:"currency"`
	At              time.Time `json:"recorded_at"`
}

type engineError struct {
	status int
	code   string
}

func (e engineError) Error() string            { return e.code }
func engineFail(status int, code string) error { return engineError{status, code} }
func engineKey(parts ...string) string         { return "gw2/" + strings.Join(parts, "/") }
func engineOpKey(m, id string) string          { return engineKey("operations", m, id) }
func engineLanePrefix(connector string) string {
	if connector == "" {
		return engineKey("dispatch") + "/"
	}
	return engineKey("connector-dispatch", connector) + "/"
}
func engineWorkKey(connector string, at time.Time, op string) string {
	return engineLanePrefix(connector) + fmt.Sprintf("%020d/%s", at.UTC().UnixMilli(), op)
}
func engineID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}
func engineHash(terms any) (string, error) {
	raw, err := json.Marshal(terms)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func engineGet(tx gwstore.Tx, m, id string) (engineOperation, error) {
	var op engineOperation
	found, err := gwstore.GetJSON(tx, engineOpKey(m, id), &op)
	if err != nil {
		return op, err
	}
	if !found {
		return op, engineFail(http.StatusNotFound, "operation_not_found")
	}
	return op, nil
}
func engineVisible(p Principal, op engineOperation) bool {
	return p.Merchant == op.Command.Merchant && p.Account == op.Command.Account && p.Outlets[op.Command.Outlet]
}

func engineSave(tx gwstore.Tx, op engineOperation) error {
	return gwstore.PutJSON(tx, engineOpKey(op.Command.Merchant, op.Command.ID), op)
}
func enginePublish(tx gwstore.Tx, op *engineOperation, when time.Time) error {
	op.Version++
	op.Updated = when.UTC()
	if err := engineSave(tx, *op); err != nil {
		return err
	}
	m := op.Command.Merchant
	counter := engineKey("feed-sequence", m)
	var sequence int64
	if _, err := gwstore.GetJSON(tx, counter, &sequence); err != nil {
		return err
	}
	if sequence == 1<<63-1 {
		return errors.New("event sequence exhausted")
	}
	sequence++
	if err := gwstore.PutJSON(tx, counter, sequence); err != nil {
		return err
	}
	c := op.Command
	event := engineEvent{PaymentMethod: c.PaymentMethod, RouteID: c.RouteID, ConnectorID: c.ConnectorID, ProviderAccount: c.ProviderAccount, CardBrand: op.CardBrand, Reference: op.Reference, Reason: op.Reason, Refunded: op.Refunded, Reserved: op.Reserved, Sequence: sequence, Operation: c.ID, Parent: c.Parent, Outlet: c.Outlet, Account: c.Account, Kind: c.Kind, Status: op.Status, Version: op.Version, Amount: c.Amount, Currency: c.Currency, At: op.Updated}
	return gwstore.PutJSON(tx, engineKey("feed", m, fmt.Sprintf("%020d", sequence)), event)
}
func engineEnqueue(tx gwstore.Tx, op *engineOperation, when time.Time) error {
	connector := op.Command.ConnectorID
	if connector != "" {
		key := engineKey("active-dispatch-lanes", connector)
		_, found, err := tx.Get(key)
		if err != nil {
			return err
		}
		if !found {
			if err := tx.Put(key, []byte(connector)); err != nil {
				return err
			}
		}
	}
	op.Ticket = engineWorkKey(connector, when, op.Command.ID)
	return gwstore.PutJSON(tx, op.Ticket, engineWork{Merchant: op.Command.Merchant, ID: op.Command.ID})
}
func engineReplay(tx gwstore.Tx, p Principal, bindingKey, digest string) (engineOperation, bool, error) {
	var b engineBinding
	found, err := gwstore.GetJSON(tx, bindingKey, &b)
	if err != nil || !found {
		return engineOperation{}, found, err
	}
	if b.Digest != digest {
		return engineOperation{}, true, engineFail(409, "idempotency_conflict")
	}
	op, err := engineGet(tx, p.Merchant, b.ID)
	if err == nil && !engineVisible(p, op) {
		return engineOperation{}, true, engineFail(404, "operation_not_found")
	}
	return op, true, err
}
func (s *Service) createIntent(ctx context.Context, p Principal, key string, in engineIntentInput) (engineOperation, bool, error) {
	if in.OrderTotal == 0 {
		in.OrderTotal = in.Amount
	}
	if in.Quote == "" {
		in.Quote = "1"
	}
	if in.PaymentMethod != "" && !engineIdentifier.MatchString(in.PaymentMethod) || !engineIdentifier.MatchString(key) || !engineIdentifier.MatchString(in.Order) || !engineIdentifier.MatchString(in.Allocation) ||
		!engineIdentifier.MatchString(in.Outlet) || !engineIdentifier.MatchString(in.Quote) || (in.Replaces != "" && !engineIdentifier.MatchString(in.Replaces)) ||
		in.Amount <= 0 || in.Amount > maxMoney || in.OrderTotal < in.Amount || in.OrderTotal > maxMoney || !engineCurrency.MatchString(in.Currency) {
		return engineOperation{}, false, engineFail(400, "invalid_intent")
	}
	switch in.Channel {
	case "terminal":
		if !engineIdentifier.MatchString(in.Terminal) {
			return engineOperation{}, false, engineFail(400, "invalid_terminal")
		}
	case "hosted":
		if in.Terminal != "" {
			return engineOperation{}, false, engineFail(400, "unexpected_terminal")
		}
	default:
		return engineOperation{}, false, engineFail(400, "invalid_channel")
	}
	id, err := engineID("pi_")
	if err != nil {
		return engineOperation{}, false, err
	}
	digest, err := engineHash(in)
	if err != nil {
		return engineOperation{}, false, err
	}
	now := s.Now().UTC()
	var result engineOperation
	var replay bool
	var candidates []railcontract.Command
	err = s.admitWithIdentity(ctx, func(tx gwstore.Tx, proof engineAttestation) error {
		var err error
		keyRow := engineKey("idempotency", p.Merchant, "charge", key)
		result, replay, err = engineReplay(tx, p, keyRow, digest)
		if err != nil || replay {
			return err
		}
		if p.Currency != in.Currency || in.Amount > p.MaxAmount || !p.Outlets[in.Outlet] || (in.Channel == "terminal" && p.Terminals[in.Terminal] != in.Outlet) {
			return engineFail(403, "program_grant_denied")
		}
		allocationKey := engineKey("allocation", p.Merchant, in.Order, in.Allocation)
		var prior string
		found, err := gwstore.GetJSON(tx, allocationKey, &prior)
		if err != nil {
			return err
		}
		if found {
			if prior != in.Replaces {
				return engineFail(409, "allocation_occupied")
			}
			old, err := engineGet(tx, p.Merchant, prior)
			if err != nil {
				return err
			}
			if old.Status != "declined" {
				return engineFail(409, "original_not_closed_unpaid")
			}
		} else if in.Replaces != "" {
			return engineFail(409, "original_not_found_for_allocation")
		}
		cmd := railcontract.Command{ID: id, Merchant: p.Merchant, Account: p.Account, Kind: "charge", Order: in.Order, Outlet: in.Outlet, Terminal: in.Terminal, Channel: in.Channel, Amount: in.Amount, Currency: in.Currency}
		if err := checkSaleAdmission(tx, p.Merchant); err != nil {
			return err
		}
		orderKey := engineKey("order", p.Merchant, in.Order)
		var order engineOrder
		found, err = gwstore.GetJSON(tx, orderKey, &order)
		if err != nil {
			return err
		}
		if !found {
			order = engineOrder{Total: in.OrderTotal, Quote: in.Quote, Currency: in.Currency, Outlet: in.Outlet, Account: p.Account}
		}
		if order.Total != in.OrderTotal || order.Quote != in.Quote || order.Currency != in.Currency || order.Outlet != in.Outlet || order.Account != p.Account {
			return engineFail(409, "order_terms_changed")
		}
		if in.Amount > order.Total-order.Captured-order.Reserved {
			return engineFail(409, "order_capacity_exceeded")
		}
		cmd.PaymentMethod = in.PaymentMethod
		if candidates == nil {
			candidates, err = s.candidateRoutes(cmd)
			if err != nil {
				return err
			}
		}
		cmd, err = chooseOpenRoute(tx, cmd, candidates, s.RouteCandidates == nil && s.SelectRoute == nil)
		if err != nil {
			return err
		}
		observed := proof.forCommand(cmd)
		if err := engineEnroll(tx, p, cmd, observed, ""); err != nil {
			return err
		}
		if in.Channel == "terminal" {
			busy := engineKey("reader", p.Merchant, in.Terminal)
			_, held, err := tx.Get(busy)
			if err != nil {
				return err
			}
			if held {
				return engineFail(409, "terminal_busy")
			}
			if err := gwstore.PutJSON(tx, busy, id); err != nil {
				return err
			}
		}
		order.Reserved += in.Amount
		if err := gwstore.PutJSON(tx, orderKey, order); err != nil {
			return err
		}
		if err := gwstore.PutJSON(tx, allocationKey, id); err != nil {
			return err
		}
		if err := gwstore.PutJSON(tx, keyRow, engineBinding{id, digest}); err != nil {
			return err
		}
		result = engineOperation{Command: cmd, Ledger: observed, Allocation: in.Allocation, Quote: in.Quote, Status: "queued", Created: now}
		if err := engineRecordAdmission(tx, result, p); err != nil {
			return err
		}
		if err := engineEnqueue(tx, &result, now); err != nil {
			return err
		}
		return enginePublish(tx, &result, now)
	})
	return result, replay, err
}
func (s *Service) createRefund(ctx context.Context, p Principal, key, parentID string, in engineRefundInput) (engineOperation, bool, error) {
	if !engineIdentifier.MatchString(key) || !engineIdentifier.MatchString(parentID) || !engineIdentifier.MatchString(in.BusinessID) || in.Amount <= 0 || in.Amount > maxMoney {
		return engineOperation{}, false, engineFail(400, "invalid_refund")
	}
	id, err := engineID("rf_")
	if err != nil {
		return engineOperation{}, false, err
	}
	digest, err := engineHash(struct {
		Parent string
		Terms  engineRefundInput
	}{parentID, in})
	if err != nil {
		return engineOperation{}, false, err
	}
	now := s.Now().UTC()
	var result engineOperation
	var replay bool
	err = s.admitWithIdentity(ctx, func(tx gwstore.Tx, proof engineAttestation) error {
		keyRow := engineKey("idempotency", p.Merchant, "refund", key)
		bizRow := engineKey("refund-reference", p.Merchant, in.BusinessID)
		var err error
		result, replay, err = engineReplay(tx, p, keyRow, digest)
		if err != nil || replay {
			return err
		}
		result, replay, err = engineReplay(tx, p, bizRow, digest)
		if err != nil {
			return err
		}
		if replay {
			return gwstore.PutJSON(tx, keyRow, engineBinding{result.Command.ID, digest})
		}
		parent, err := engineGet(tx, p.Merchant, parentID)
		if err != nil {
			return err
		}
		if parent.Command.Kind != "charge" || !engineVisible(p, parent) {
			return engineFail(404, "operation_not_found")
		}
		if parent.Status != "captured" {
			return engineFail(409, "original_not_captured")
		}
		held, err := releaseFinancialHold(tx, parent.Command, parent.Reference)
		if err != nil {
			return err
		}
		if held {
			return engineFail(409, "original_financial_exception")
		}
		if in.Amount > p.MaxAmount {
			return engineFail(403, "program_grant_denied")
		}
		if in.Amount > parent.Command.Amount-parent.Refunded-parent.Reserved {
			return engineFail(409, "insufficient_refundable_amount")
		}
		if !engineLedgerID.MatchString(parent.Ledger) {
			return engineFail(409, "legacy_processor_enrollment_required")
		}
		if err := engineEnroll(tx, p, parent.Command, proof.forCommand(parent.Command), parent.Ledger); err != nil {
			return err
		}
		parent.Reserved += in.Amount
		if err := enginePublish(tx, &parent, now); err != nil {
			return err
		}
		c := parent.Command
		c.ID = id
		c.Parent = parentID
		c.Kind = "refund"
		c.Amount = in.Amount
		result = engineOperation{Command: c, Ledger: parent.Ledger, Allocation: parent.Allocation, Quote: parent.Quote, RefundKey: in.BusinessID, Status: "queued", Created: now}
		for _, binding := range []string{keyRow, bizRow} {
			if err := gwstore.PutJSON(tx, binding, engineBinding{id, digest}); err != nil {
				return err
			}
		}
		if err := engineRecordAdmission(tx, result, p); err != nil {
			return err
		}
		if err := engineEnqueue(tx, &result, now); err != nil {
			return err
		}
		return enginePublish(tx, &result, now)
	})
	return result, replay, err
}

package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"dakimpay/internal/gwstore"
)

type engineView struct {
	PaymentMethod   string    `json:"payment_method,omitempty"`
	RouteID         string    `json:"route_id,omitempty"`
	ConnectorID     string    `json:"connector_id,omitempty"`
	ProviderAccount string    `json:"provider_account,omitempty"`
	CardBrand       string    `json:"card_brand,omitempty"`
	ID              string    `json:"id"`
	Operation       string    `json:"operation_id"`
	Merchant        string    `json:"merchant_id"`
	Account         string    `json:"processor_account"`
	Kind            string    `json:"kind"`
	Parent          string    `json:"parent_id,omitempty"`
	Order           string    `json:"order_id"`
	Allocation      string    `json:"allocation_id"`
	Quote           string    `json:"quote_version"`
	Outlet          string    `json:"outlet_id"`
	Terminal        string    `json:"terminal_id,omitempty"`
	Channel         string    `json:"channel"`
	Amount          int64     `json:"amount_minor"`
	Currency        string    `json:"currency"`
	Status          string    `json:"status"`
	Reference       string    `json:"reference,omitempty"`
	CheckoutURL     string    `json:"checkout_url,omitempty"`
	BusinessRefund  string    `json:"merchant_refund_id,omitempty"`
	Refunded        int64     `json:"refunded_minor"`
	Reserved        int64     `json:"reserved_refund_minor"`
	Reason          string    `json:"reason,omitempty"`
	Version         int64     `json:"version"`
	Created         time.Time `json:"created_at"`
	Updated         time.Time `json:"updated_at"`
}

func enginePresent(op engineOperation) engineView {
	c := op.Command
	return engineView{PaymentMethod: c.PaymentMethod, RouteID: c.RouteID, ConnectorID: c.ConnectorID, ProviderAccount: c.ProviderAccount, CardBrand: op.CardBrand, ID: c.ID, Operation: c.ID, Merchant: c.Merchant, Account: c.Account, Kind: c.Kind, Parent: c.Parent, Order: c.Order, Allocation: op.Allocation, Quote: op.Quote, Outlet: c.Outlet, Terminal: c.Terminal, Channel: c.Channel, Amount: c.Amount, Currency: c.Currency, Status: op.Status, Reference: op.Reference, CheckoutURL: op.CheckoutURL, BusinessRefund: op.RefundKey, Refunded: op.Refunded, Reserved: op.Reserved, Reason: op.Reason, Version: op.Version, Created: op.Created, Updated: op.Updated}
}

// Accept one flat command object with exact field names. Duplicate or
// case-aliased keys would otherwise make a signed economic request ambiguous.
func engineDecode(r *http.Request, out any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16385))
	if err != nil || len(raw) > 16384 {
		return engineFail(400, "invalid_command_body")
	}
	typ := reflect.TypeOf(out).Elem()
	allowed := make(map[string]bool, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		allowed[strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return engineFail(400, "invalid_command_body")
	}
	for d.More() {
		tok, err = d.Token()
		name, ok := tok.(string)
		if err != nil || !ok || !allowed[name] {
			return engineFail(400, "unknown_or_duplicate_field")
		}
		delete(allowed, name)
		var value json.RawMessage
		if err = d.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return engineFail(400, "invalid_field_value")
		}
	}
	if _, err = d.Token(); err != nil {
		return engineFail(400, "invalid_command_body")
	}
	if _, err = d.Token(); err != io.EOF {
		return engineFail(400, "invalid_command_body")
	}
	if err = json.Unmarshal(raw, out); err != nil {
		return engineFail(400, "invalid_field_value")
	}
	return nil
}
func engineSend(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func engineSendError(w http.ResponseWriter, err error) {
	var failure engineError
	if !errors.As(err, &failure) {
		log.Printf("gateway payment request failed: %v", err)
		failure = engineError{503, "gateway_unavailable"}
	}
	engineSend(w, failure.status, map[string]string{"error": failure.code})
}
func engineMerchant(h func(http.ResponseWriter, *http.Request, Principal) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok {
			engineSendError(w, engineFail(401, "authentication_required"))
			return
		}
		if p.Role != MerchantRole || !engineIdentifier.MatchString(p.Merchant) || !engineIdentifier.MatchString(p.Account) {
			engineSendError(w, engineFail(403, "merchant_credential_required"))
			return
		}
		if err := h(w, r, p); err != nil {
			engineSendError(w, err)
		}
	}
}
func engineIdempotency(r *http.Request) (string, error) {
	k := r.Header.Values("Idempotency-Key")
	if len(k) != 1 || !engineIdentifier.MatchString(k[0]) {
		return "", engineFail(400, "invalid_idempotency_key")
	}
	return k[0], nil
}
func (s *Service) registerPayments(m *http.ServeMux) {
	s.registerInquiry(m)
	m.HandleFunc("GET /v2/payment-intents/{id}/release-evidence", engineMerchant(s.releaseEvidence))
	m.HandleFunc("GET /v2/payment-intents/{id}/release-eligibility", engineMerchant(s.releaseEvidence))
	m.HandleFunc("POST /v2/payment-intents", engineMerchant(func(w http.ResponseWriter, r *http.Request, p Principal) error {
		key, err := engineIdempotency(r)
		if err != nil {
			return err
		}
		var in engineIntentInput
		if err := engineDecode(r, &in); err != nil {
			return err
		}
		op, replay, err := s.createIntent(r.Context(), p, key, in)
		if err != nil {
			return err
		}
		code := 201
		if replay {
			code = 200
		}
		w.Header().Set("Location", "/v2/payment-intents/"+op.Command.ID)
		engineSend(w, code, enginePresent(op))
		return nil
	}))
	m.HandleFunc("POST /v2/payment-intents/{id}/refunds", engineMerchant(func(w http.ResponseWriter, r *http.Request, p Principal) error {
		key, err := engineIdempotency(r)
		if err != nil {
			return err
		}
		var in engineRefundInput
		if err := engineDecode(r, &in); err != nil {
			return err
		}
		op, replay, err := s.createRefund(r.Context(), p, key, r.PathValue("id"), in)
		if err != nil {
			return err
		}
		code := 201
		if replay {
			code = 200
		}
		w.Header().Set("Location", "/v2/refunds/"+op.Command.ID)
		engineSend(w, code, enginePresent(op))
		return nil
	}))
	get := func(kind string) http.HandlerFunc {
		return engineMerchant(func(w http.ResponseWriter, r *http.Request, p Principal) error {
			id := r.PathValue("id")
			if !engineIdentifier.MatchString(id) {
				return engineFail(404, "operation_not_found")
			}
			var op engineOperation
			err := s.Store.View(r.Context(), func(tx gwstore.Tx) error {
				var err error
				op, err = engineGet(tx, p.Merchant, id)
				if err == nil && (!engineVisible(p, op) || (kind != "" && op.Command.Kind != kind)) {
					return engineFail(404, "operation_not_found")
				}
				return err
			})
			if err != nil {
				return err
			}
			engineSend(w, 200, enginePresent(op))
			return nil
		})
	}
	m.HandleFunc("GET /v2/payment-intents/{id}", get("charge"))
	m.HandleFunc("GET /v2/refunds/{id}", get("refund"))
	m.HandleFunc("GET /v2/operations/{id}", get(""))
	m.HandleFunc("GET /v2/events", engineMerchant(s.readEvents))
}
func (s *Service) readEvents(w http.ResponseWriter, r *http.Request, p Principal) error {
	query := r.URL.Query()
	if len(query["limit"]) > 1 || len(query["after"]) > 1 {
		return engineFail(400, "invalid_page")
	}
	after := int64(0)
	limit := 50
	var err error
	if raw := query.Get("after"); raw != "" {
		after, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || after < 0 {
			return engineFail(400, "invalid_cursor")
		}
	}
	if raw := query.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			return engineFail(400, "invalid_limit")
		}
	}
	prefix := engineKey("feed", p.Merchant) + "/"
	events := make([]engineEvent, 0, limit)
	next := after
	err = s.Store.View(r.Context(), func(tx gwstore.Tx) error {
		events = events[:0]
		next = after
		rows, err := tx.Scan(prefix, prefix+fmt.Sprintf("%020d", after), limit)
		if err != nil {
			return err
		}
		for _, row := range rows {
			var e engineEvent
			if err := json.Unmarshal(row.Value, &e); err != nil {
				return err
			}
			next = e.Sequence
			if p.Account == e.Account && p.Outlets[e.Outlet] {
				events = append(events, e)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	engineSend(w, 200, struct {
		Events []engineEvent `json:"events"`
		Cursor string        `json:"next_cursor"`
	}{events, strconv.FormatInt(next, 10)})
	return nil
}

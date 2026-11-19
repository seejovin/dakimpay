package pay

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type PaymentInput struct {
	OrderID      string `json:"order_id"`
	Channel      string `json:"channel"`
	Amount       int64  `json:"amount"`
	Currency     string `json:"currency"`
	Brand        string `json:"brand"`
	TerminalID   string `json:"terminal_id"`
	LoseResponse bool   `json:"lose_response"`
}
type Payment struct {
	ID         string `json:"id"`
	MerchantID string `json:"merchant_id"`
	PaymentInput
	Status      string    `json:"status"`
	Reference   string    `json:"reference"`
	CheckoutURL string    `json:"checkout_url"`
	Refunded    int64     `json:"refunded"`
	CreatedAt   time.Time `json:"created_at"`
}

const paymentColumns = `p.id,p.merchant_id,p.order_id,p.channel,p.amount,p.currency,p.brand,p.terminal_id,p.lose_response,p.status,coalesce(p.reference,''),p.checkout_url,coalesce((select sum(r.amount) from pay.refunds r where r.payment_id=p.id and r.status='succeeded'),0),p.created_at`

func readPayment(row pgx.Row) (Payment, error) {
	var p Payment
	err := row.Scan(&p.ID, &p.MerchantID, &p.OrderID, &p.Channel, &p.Amount, &p.Currency, &p.Brand, &p.TerminalID, &p.LoseResponse, &p.Status, &p.Reference, &p.CheckoutURL, &p.Refunded, &p.CreatedAt)
	return p, err
}
func payment(ctx context.Context, q sqlDB, id string) (Payment, error) {
	return readPayment(q.QueryRow(ctx, "SELECT "+paymentColumns+" FROM pay.payments p WHERE p.id=$1 AND p.merchant_id='mimi'", id))
}
func (in PaymentInput) validate() error {
	if !identifier.MatchString(in.OrderID) || in.Currency != "USD" || in.Amount < 1 || in.Amount > 100000000 {
		return fail(422, "Supply an order reference, USD, and a positive integer amount within the simulator limit.")
	}
	if in.Brand != "visa" && in.Brand != "mastercard" && in.Brand != "amex" {
		return fail(422, "Select a supported simulated card brand.")
	}
	switch in.Channel {
	case "terminal":
		if in.TerminalID != "MIMI-01" {
			return fail(422, "Use Mimi’s enrolled terminal MIMI-01.")
		}
	case "online":
		if in.TerminalID != "" || in.LoseResponse {
			return fail(422, "Online checkout does not accept terminal controls.")
		}
	default:
		return fail(422, "Select terminal or online checkout.")
	}
	return nil
}
func (s *Service) createPayment(w http.ResponseWriter, r *http.Request) error {
	k, err := key(r)
	if err != nil {
		return err
	}
	var in PaymentInput
	if err = decode(r, &in); err != nil {
		return err
	}
	if err = in.validate(); err != nil {
		return err
	}
	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	id := newID("dp")
	tag, err := tx.Exec(ctx, `INSERT INTO pay.payments(id,merchant_id,request_key,order_id,channel,amount,currency,brand,terminal_id,lose_response)
 VALUES($1,'mimi',$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, id, k, in.OrderID, in.Channel, in.Amount, in.Currency, in.Brand, in.TerminalID, in.LoseResponse)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		old, err := readPayment(tx.QueryRow(ctx, "SELECT "+paymentColumns+" FROM pay.payments p WHERE p.merchant_id='mimi' AND p.request_key=$1", k))
		if errors.Is(err, pgx.ErrNoRows) {
			return fail(409, "This merchant order already has a payment. Inspect its original operation.")
		}
		if err != nil {
			return err
		}
		if old.PaymentInput != in {
			return fail(409, "This idempotency key is bound to a different payment.")
		}
		answer(w, 200, old)
		return nil
	}
	if err = note(ctx, tx, id, "created", "Merchant order recorded before acquirer dispatch."); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	p, err := payment(ctx, s.db, id)
	if err != nil {
		return err
	}
	p, err = s.syncCharge(ctx, p, false)
	if err != nil {
		return err
	}
	status := 201
	if p.Status == "unknown" {
		status = 202
	}
	answer(w, status, p)
	return nil
}
func (s *Service) getPayment(w http.ResponseWriter, r *http.Request) error {
	p, err := payment(r.Context(), s.db, r.PathValue("id"))
	if err != nil {
		return err
	}
	answer(w, 200, p)
	return nil
}
func (s *Service) recover(w http.ResponseWriter, r *http.Request) error {
	p, err := payment(r.Context(), s.db, r.PathValue("id"))
	if err != nil {
		return err
	}
	if p.Status != "captured" {
		p, err = s.syncCharge(r.Context(), p, true)
		if err != nil {
			return err
		}
	}
	answer(w, 200, p)
	return nil
}

type chargeFact struct {
	OperationID string `json:"operation_id"`
	Reference   string `json:"reference"`
	MerchantID  string `json:"merchant_id"`
	Status      string `json:"status"`
	OrderID     string `json:"order_id"`
	Channel     string `json:"channel"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	Brand       string `json:"brand"`
	TerminalID  string `json:"terminal_id"`
	CheckoutURL string `json:"checkout_url"`
}

func (s *Service) syncCharge(ctx context.Context, p Payment, inspect bool) (Payment, error) {
	var fact chargeFact
	var code int
	var err error
	if inspect {
		code, err = s.remote(ctx, s.acquirer, http.MethodGet, "/sim/charges/"+url.PathEscape(p.ID), nil, &fact)
	}
	if !inspect || code == 404 {
		req := struct {
			OperationID string `json:"operation_id"`
			MerchantID  string `json:"merchant_id"`
			PaymentInput
		}{p.ID, p.MerchantID, p.PaymentInput}
		code, err = s.remote(ctx, s.acquirer, http.MethodPost, "/sim/charges", req, &fact)
	}
	if err != nil {
		if code == 401 || code == 403 || code == 400 || code == 422 || code == 409 {
			return p, fail(502, "Acquirer rejected the stored operation. Inspect the original reference.")
		}
		_, writeErr := s.db.Exec(ctx, "INSERT INTO pay.events(subject,kind,message) VALUES ($1,'unknown','Acquirer reply unavailable. Check the original operation before collecting again.') ON CONFLICT DO NOTHING", p.ID)
		if writeErr != nil {
			return p, writeErr
		}
		return payment(ctx, s.db, p.ID)
	}
	if fact.OperationID != p.ID || fact.MerchantID != p.MerchantID || fact.OrderID != p.OrderID || fact.Channel != p.Channel || fact.Amount != p.Amount || fact.Currency != p.Currency || fact.Brand != p.Brand || fact.TerminalID != p.TerminalID || !identifier.MatchString(fact.Reference) {
		return p, fail(502, "Acquirer evidence differs from the original payment.")
	}
	if fact.Status != "captured" && (fact.Status != "awaiting_customer" || p.Channel != "online") {
		return p, fail(502, "Unrecognized acquirer state.")
	}
	link := ""
	if fact.Status == "awaiting_customer" {
		base, _ := url.Parse(s.acquirer)
		target, err := url.Parse(fact.CheckoutURL)
		if err != nil || fact.CheckoutURL == "" {
			return p, fail(502, "Invalid acquirer checkout destination.")
		}
		target = base.ResolveReference(target)
		if target.Scheme != base.Scheme || !strings.EqualFold(target.Host, base.Host) || target.User != nil {
			return p, fail(502, "Untrusted acquirer checkout destination.")
		}
		link = target.String()
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return p, err
	}
	defer tx.Rollback(ctx)
	var status, ref, priorLink string
	if err = tx.QueryRow(ctx, "SELECT status,coalesce(reference,''),checkout_url FROM pay.payments WHERE id=$1 FOR UPDATE", p.ID).Scan(&status, &ref, &priorLink); err != nil {
		return p, err
	}
	if ref != "" && ref != fact.Reference {
		return p, fail(502, "Acquirer changed the original payment reference.")
	}
	if status != "captured" && (status != fact.Status || ref != fact.Reference || priorLink != link) {
		if _, err = tx.Exec(ctx, "UPDATE pay.payments SET status=$2,reference=$3,checkout_url=$4 WHERE id=$1", p.ID, fact.Status, fact.Reference, link); err != nil {
			return p, err
		}
		msg := "Partner-hosted checkout created; awaiting the customer."
		if fact.Status == "captured" {
			msg = "Acquirer capture verified against order, merchant, currency and amount."
		}
		if inspect && fact.Status == "captured" {
			if err = note(ctx, tx, p.ID, "verified", "Original acquirer operation inspected and capture confirmed."); err != nil {
				return p, err
			}
		}
		if err = note(ctx, tx, p.ID, fact.Status, msg); err != nil {
			return p, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return p, err
	}
	return payment(ctx, s.db, p.ID)
}

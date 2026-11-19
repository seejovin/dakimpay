package pay

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"time"
)

type Refund struct {
	ID                string    `json:"id"`
	MerchantID        string    `json:"merchant_id"`
	MerchantOperation string    `json:"merchant_operation"`
	PaymentID         string    `json:"payment_id"`
	Amount            int64     `json:"amount"`
	Currency          string    `json:"currency"`
	Reference         string    `json:"reference"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
}

const refundColumns = `r.id,p.merchant_id,r.request_key,r.payment_id,r.amount,r.currency,coalesce(r.reference,''),r.status,r.created_at`
const refundFrom = ` FROM pay.refunds r JOIN pay.payments p ON p.id=r.payment_id WHERE p.merchant_id='mimi'`

func readRefund(row pgx.Row) (Refund, error) {
	var f Refund
	err := row.Scan(&f.ID, &f.MerchantID, &f.MerchantOperation, &f.PaymentID, &f.Amount, &f.Currency, &f.Reference, &f.Status, &f.CreatedAt)
	return f, err
}
func refundByID(ctx context.Context, q sqlDB, id string) (Refund, error) {
	return readRefund(q.QueryRow(ctx, "SELECT "+refundColumns+refundFrom+" AND r.id=$1", id))
}
func (s *Service) getRefund(w http.ResponseWriter, r *http.Request) error {
	f, err := refundByID(r.Context(), s.db, r.PathValue("id"))
	if err != nil {
		return err
	}
	answer(w, 200, f)
	return nil
}
func (s *Service) recoverRefund(w http.ResponseWriter, r *http.Request) error {
	f, err := refundByID(r.Context(), s.db, r.PathValue("id"))
	if err != nil {
		return err
	}
	if f.Status != "succeeded" {
		f, err = s.syncRefund(r.Context(), f)
		if err != nil {
			return err
		}
	}
	answer(w, 200, f)
	return nil
}
func (s *Service) refund(w http.ResponseWriter, r *http.Request) error {
	k, err := key(r)
	if err != nil {
		return err
	}
	var in struct {
		Amount int64 `json:"amount"`
	}
	if err = decode(r, &in); err != nil {
		return err
	}
	if in.Amount < 1 || in.Amount > 100000000 {
		return fail(422, "Refund amount must be positive and within the original sale.")
	}
	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	pid := r.PathValue("id")
	var amount int64
	var status string
	if err = tx.QueryRow(ctx, "SELECT amount,status FROM pay.payments WHERE id=$1 AND merchant_id='mimi' FOR UPDATE", pid).Scan(&amount, &status); err != nil {
		return err
	}
	old, err := readRefund(tx.QueryRow(ctx, "SELECT "+refundColumns+refundFrom+" AND r.request_key=$1", k))
	if err == nil {
		if old.PaymentID != pid || old.Amount != in.Amount {
			return fail(409, "Refund key already belongs to different terms.")
		}
		answer(w, 200, old)
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if status != "captured" {
		return fail(409, "Verify capture before requesting a refund.")
	}
	var reserved int64
	if err = tx.QueryRow(ctx, "SELECT coalesce(sum(amount),0) FROM pay.refunds WHERE payment_id=$1", pid).Scan(&reserved); err != nil {
		return err
	}
	if in.Amount > amount-reserved {
		return fail(409, "Refund exceeds the unreserved balance of the original payment.")
	}
	id := newID("dr")
	_, err = tx.Exec(ctx, "INSERT INTO pay.refunds(id,request_key,payment_id,amount,currency) VALUES($1,$2,$3,$4,'USD')", id, k, pid, in.Amount)
	if err != nil {
		return err
	}
	if err = note(ctx, tx, id, "reserved", "Original payment balance reserved before refund dispatch."); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	f, err := refundByID(ctx, s.db, id)
	if err != nil {
		return err
	}
	f, err = s.syncRefund(ctx, f)
	if err != nil {
		return err
	}
	statusCode := 201
	if f.Status == "unknown" {
		statusCode = 202
	}
	answer(w, statusCode, f)
	return nil
}
func (s *Service) syncRefund(ctx context.Context, f Refund) (Refund, error) {
	p, err := payment(ctx, s.db, f.PaymentID)
	if err != nil {
		return f, err
	}
	type refundRequest struct {
		OperationID     string `json:"operation_id"`
		MerchantID      string `json:"merchant_id"`
		ChargeReference string `json:"charge_reference"`
		Amount          int64  `json:"amount"`
		Currency        string `json:"currency"`
	}
	req := refundRequest{f.ID, f.MerchantID, p.Reference, f.Amount, f.Currency}
	var fact struct {
		refundRequest
		Reference string `json:"reference"`
		Status    string `json:"status"`
	}
	code, err := s.remote(ctx, s.acquirer, http.MethodPost, "/sim/refunds", req, &fact)
	if err != nil {
		if code == 400 || code == 401 || code == 403 || code == 409 || code == 422 {
			return f, fail(502, "Refund could not be verified with the acquirer. Inspect the original request.")
		}
		_, writeErr := s.db.Exec(ctx, "INSERT INTO pay.events(subject,kind,message) VALUES ($1,'unknown','Refund reply unavailable. The original amount remains reserved.') ON CONFLICT DO NOTHING", f.ID)
		return f, writeErr
	}
	if fact.refundRequest != req || fact.Status != "succeeded" || !identifier.MatchString(fact.Reference) {
		return f, fail(502, "Refund evidence differs from the original operation.")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return f, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE pay.refunds SET reference=$2,status='succeeded' WHERE id=$1 AND (reference IS NULL OR reference=$2)`, f.ID, fact.Reference)
	if err != nil {
		return f, err
	}
	if tag.RowsAffected() != 1 {
		return f, fail(502, "Acquirer changed a confirmed refund reference.")
	}
	if err = note(ctx, tx, f.ID, "succeeded", "Refund confirmed against the original captured card payment."); err != nil {
		return f, err
	}
	if err = tx.Commit(ctx); err != nil {
		return f, err
	}
	return refundByID(ctx, s.db, f.ID)
}

package pay

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
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
	SourceOperation   string    `json:"source_operation,omitempty"`
}

const refundColumns = `r.id,p.merchant_id,coalesce(r.request_key,r.source_operation),r.payment_id,r.amount,r.currency,coalesce(r.reference,''),r.status,r.created_at,coalesce(r.source_operation,'')`
const refundFrom = ` FROM pay.refunds r JOIN pay.payments p ON p.id=r.payment_id WHERE p.merchant_id='mimi'`

func readRefund(row pgx.Row) (Refund, error) {
	var f Refund
	err := row.Scan(&f.ID, &f.MerchantID, &f.MerchantOperation, &f.PaymentID, &f.Amount, &f.Currency, &f.Reference, &f.Status, &f.CreatedAt, &f.SourceOperation)
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
	if f.Status == "unknown" {
		f, err = s.syncRefund(r.Context(), f, true)
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
	var currency, status string
	if err = tx.QueryRow(ctx, "SELECT currency,status FROM pay.payments WHERE id=$1 AND merchant_id='mimi' FOR UPDATE", pid).Scan(&currency, &status); err != nil {
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
	var available int64
	var review bool
	if err = tx.QueryRow(ctx, "SELECT available,review FROM pay.refund_balances WHERE payment_id=$1", pid).Scan(&available, &review); err != nil {
		return err
	}
	if review {
		return fail(409, "Refund capacity is under review. Resolve the original refund evidence before reserving more.")
	}
	if in.Amount > available {
		return fail(409, "Refund exceeds the unreserved balance of the original payment.")
	}
	id := newID("dr")
	_, err = tx.Exec(ctx, "INSERT INTO pay.refunds(id,request_key,payment_id,amount,currency) VALUES($1,$2,$3,$4,$5)", id, k, pid, in.Amount, currency)
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
	f, err = s.syncRefund(ctx, f, false)
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

type refundRequest struct {
	OperationID     string `json:"operation_id"`
	MerchantID      string `json:"merchant_id"`
	ChargeReference string `json:"charge_reference"`
	Amount          int64  `json:"amount"`
	Currency        string `json:"currency"`
}
type refundFact struct {
	refundRequest
	Reference string `json:"reference"`
	Status    string `json:"status"`
}

// A reservation can be released on an independently retrieved definitive
// refusal only when its exact fields were supplied once by the processor.
type refundDisposition struct {
	refundRequest
	Reference *string `json:"reference"`
	Status    string  `json:"status"`
}

func (f *refundDisposition) UnmarshalJSON(raw []byte) error {
	type plain refundDisposition
	return decodeEvidence(raw, (*plain)(f))
}

func originalRefund(f Refund, parent Payment) refundRequest {
	operation := f.ID
	if f.SourceOperation != "" {
		operation = f.SourceOperation
	}
	return refundRequest{operation, f.MerchantID, parent.Reference, f.Amount, f.Currency}
}

// Recovery first asks for the original operation. Only an absent operation at
// this idempotent simulator can resume the exact same instruction.
func (s *Service) syncRefund(ctx context.Context, f Refund, inspect bool) (Refund, error) {
	if f.SourceOperation != "" || f.Status != "unknown" {
		return f, nil
	}
	p, err := payment(ctx, s.db, f.PaymentID)
	if err != nil {
		return f, err
	}
	req := originalRefund(f, p)
	if inspect {
		result, code, err := s.inquireRefund(ctx, f, req)
		if code != http.StatusNotFound {
			return result, err
		}
	}

	var reply refundDisposition
	code, err := s.remote(ctx, s.acquirer, http.MethodPost, "/sim/refunds", req, &reply)
	if err == nil && reply.Status == "succeeded" {
		return s.applyRefundOutcome(ctx, f, req, reply)
	}
	if code == http.StatusConflict || (err == nil && reply.refundRequest == req && reply.Status == "declined") {
		// A terminal failure of the original request has to be confirmed by
		// the independent lookup before its funds can be reserved again.
		result, _, lookupErr := s.inquireRefund(ctx, f, req)
		return result, lookupErr
	}
	if err == nil || (code >= 200 && code < 500) {
		return f, fail(502, "Refund could not be verified with the acquirer. Inspect the original request.")
	}
	return s.refundUnresolved(ctx, f)
}

func (s *Service) inquireRefund(ctx context.Context, f Refund, req refundRequest) (Refund, int, error) {
	var observed refundDisposition
	code, err := s.remote(ctx, s.acquirer, http.MethodGet, "/sim/refunds/"+url.PathEscape(f.ID), nil, &observed)
	if err != nil {
		if code == 0 || code >= 500 {
			current, writeErr := s.refundUnresolved(ctx, f)
			return current, code, writeErr
		}
		return f, code, fail(502, "No valid authoritative outcome for this refund. Its reservation is still held.")
	}
	current, err := s.applyRefundOutcome(ctx, f, req, observed)
	return current, code, err
}

func (s *Service) applyRefundOutcome(ctx context.Context, f Refund, req refundRequest, fact refundDisposition) (Refund, error) {
	if fact.refundRequest == req && fact.Reference != nil {
		if fact.Status == "declined" && *fact.Reference == "" {
			return s.declineRefund(ctx, f)
		}
		if fact.Status == "succeeded" {
			return s.acceptRefund(ctx, f, req, refundFact{fact.refundRequest, *fact.Reference, fact.Status})
		}
	}
	return f, fail(502, "Refund evidence differs from the original operation.")
}

func (s *Service) refundUnresolved(ctx context.Context, f Refund) (Refund, error) {
	if err := note(ctx, s.db, f.ID, "unknown", "Refund reply unavailable. Inspect the original operation."); err != nil {
		return f, err
	}
	return refundByID(ctx, s.db, f.ID)
}

// Source observations and merchant confirmations share the provider's refund
// reference namespace, including when they point at two different captures.
func lockRefundReference(ctx context.Context, tx pgx.Tx, ref string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('pay:refund-reference:' || $1, 0))", ref)
	return err
}

func (s *Service) declineRefund(ctx context.Context, f Refund) (Refund, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return f, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT 1 FROM pay.payments WHERE id=$1 FOR UPDATE", f.PaymentID); err != nil {
		return f, err
	}
	tag, err := tx.Exec(ctx, `UPDATE pay.refunds SET status='declined' WHERE id=$1
		AND reference IS NULL AND source_operation IS NULL AND status IN ('unknown','declined')`, f.ID)
	if err != nil {
		return f, err
	}
	if tag.RowsAffected() != 1 {
		return f, fail(409, "The processor refusal conflicts with a confirmed refund.")
	}
	if err = note(ctx, tx, f.ID, "declined", "Processor confirmed refusal of the original refund. Reservation released."); err != nil {
		return f, err
	}
	if err = tx.Commit(ctx); err != nil {
		return f, err
	}
	return refundByID(ctx, s.db, f.ID)
}

func (s *Service) acceptRefund(ctx context.Context, f Refund, req refundRequest, fact refundFact) (Refund, error) {
	if fact.refundRequest != req || fact.Status != "succeeded" || !identifier.MatchString(fact.Reference) {
		return f, fail(502, "Refund evidence differs from the original operation.")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return f, err
	}
	defer tx.Rollback(ctx)
	// A provider-originated refund can arrive while this merchant refund is
	// in flight. Check the confirmed total under the shared capture row lock.
	var captured int64
	if err = tx.QueryRow(ctx, "SELECT amount FROM pay.payments WHERE id=$1 AND merchant_id=$2 AND currency=$3 AND status='captured' FOR UPDATE", f.PaymentID, f.MerchantID, f.Currency).Scan(&captured); err != nil {
		return f, err
	}
	if err = lockRefundReference(ctx, tx, fact.Reference); err != nil {
		return f, err
	}
	var otherConfirmed, heldAtSource int64
	var disputedReference bool
	if err = tx.QueryRow(ctx, `SELECT
		(SELECT coalesce(sum(amount),0) FROM pay.refunds WHERE payment_id=$1 AND id<>$2 AND status='succeeded'),
		(SELECT coalesce(sum(amount),0) FROM pay.refund_source_holds WHERE payment_id=$1),
		EXISTS(SELECT FROM pay.refund_source_holds WHERE reference=$3)`,
		f.PaymentID, f.ID, fact.Reference).Scan(&otherConfirmed, &heldAtSource, &disputedReference); err != nil {
		return f, err
	}
	if disputedReference || f.Amount > captured-otherConfirmed-heldAtSource {
		return f, fail(409, "Refund confirmation conflicts with independently observed refunds on the original capture.")
	}
	tag, err := tx.Exec(ctx, `UPDATE pay.refunds SET reference=$2,status='succeeded' WHERE id=$1 AND status<>'declined' AND source_operation IS NULL AND (reference IS NULL OR reference=$2)`, f.ID, fact.Reference)
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

package pay

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// A verified provider movement has a different identity from the merchant's
// refund instructions. Its operation and reference remain bound on every replay.
type sourceRefund struct {
	refundFact
	PaymentID  string    `json:"payment_id"`
	Origin     string    `json:"origin"`
	OccurredAt time.Time `json:"occurred_at"`
}

func (f *sourceRefund) UnmarshalJSON(raw []byte) error {
	type plain sourceRefund
	return decodeEvidence(raw, (*plain)(f))
}

// The simulator connector translates its lookup methods into one normalized
// refund observation. The finance workflow consumes the same facts for either
// discovery path regardless of the processor's wire format.
func (s *Service) lookupSimulatorRefund(ctx context.Context, operation, reference string) (sourceRefund, bool, error) {
	path := "/sim/source-refunds/" + url.PathEscape(operation)
	if reference != "" {
		path = "/sim/source-refunds/by-reference/" + url.PathEscape(reference)
	}
	var f sourceRefund
	code, err := s.remote(ctx, s.acquirer, http.MethodGet, path, nil, &f)
	if code == http.StatusNotFound {
		return f, false, nil
	}
	return f, err == nil, err
}

// An authenticated callback and an unmatched statement line are both discovery
// signals. Either one has to retrieve the independently stored processor fact.
func (s *Service) inspectSourceRefund(ctx context.Context, operation, reference string) (string, string, error) {
	f, found, err := s.lookupRefund(ctx, operation, reference)
	if err != nil {
		return "pending", "Original provider refund evidence unavailable or unreadable. Retry the original lookup.", nil
	}
	if !found {
		return "pending", "No authoritative source refund found. The original movement remains unverified.", nil
	}
	if !identifier.MatchString(f.OperationID) || !identifier.MatchString(f.Reference) || !identifier.MatchString(f.PaymentID) ||
		!identifier.MatchString(f.ChargeReference) || (operation != "" && operation != f.OperationID) ||
		(reference != "" && reference != f.Reference) || f.MerchantID != "mimi" || f.Currency != "USD" || f.Origin != "acquirer" ||
		f.Status != "succeeded" || f.Amount <= 0 || f.OccurredAt.IsZero() {
		return "exception", "Provider refund identity, terms or status did not match the source lookup.", nil
	}
	return s.recordSourceRefund(ctx, f)
}

func (s *Service) recordSourceRefund(ctx context.Context, f sourceRefund) (string, string, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	// Sequences alone can commit out of order. Serialize the allocation and
	// commit of this merchant's feed positions so a cursor never skips a record.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('pay:adjustments:mimi'))"); err != nil {
		return "", "", err
	}
	p, err := readPayment(tx.QueryRow(ctx, paymentSelect+` WHERE p.id=$1 AND p.merchant_id=$2 FOR UPDATE OF p`, f.PaymentID, f.MerchantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return "exception", "Source refund refers to an unknown original merchant payment.", nil
	}
	if err != nil {
		return "", "", err
	}
	if p.Status != "captured" || p.Reference != f.ChargeReference || p.Currency != f.Currency || f.Amount > p.Amount {
		return "exception", "Original captured amount, currency or provider reference differs.", nil
	}
	if err = lockRefundReference(ctx, tx, f.Reference); err != nil {
		return "", "", err
	}
	identity, err := lookupSourceIdentity(ctx, tx, f)
	if err != nil {
		return "", "", err
	}
	if identity.count > 1 || !identity.same {
		return "exception", "The original provider refund identity was reused with different terms.", nil
	}
	var originalPayout string
	err = tx.QueryRow(ctx, `SELECT b.id FROM pay.lines l JOIN pay.payouts b ON b.id=l.payout_id
		WHERE l.kind='capture' AND l.reference=$1 AND l.amount=$2 AND b.currency=$3
		AND b.status='matched' AND b.bank_evidence IS NOT NULL`, p.Reference, p.Amount, p.Currency).Scan(&originalPayout)
	if errors.Is(err, pgx.ErrNoRows) {
		return "pending", "Waiting for the original capture's independently matched bank deposit.", nil
	}
	if err != nil {
		return "", "", err
	}
	if identity.count == 1 && identity.originalPayout != originalPayout {
		return "exception", "Provider refund refers to a different original deposit.", nil
	}
	if identity.published {
		return "applied", "Previously verified provider refund and original deposit matched.", nil
	}
	// Posted refunds and other verified provider movements represent real
	// money. Unknown merchant instructions retain their full reservations
	// until an independent inquiry resolves each original operation.
	var confirmed, pending, otherHeld int64
	err = tx.QueryRow(ctx, `SELECT coalesce(sum(amount) FILTER (WHERE status='succeeded'),0),
	    coalesce(sum(amount) FILTER (WHERE status='unknown'),0),
	    (SELECT coalesce(sum(amount),0) FROM pay.refund_source_holds WHERE payment_id=$1 AND source_operation<>$2)
	    FROM pay.refunds WHERE payment_id=$1`, p.ID, f.OperationID).Scan(&confirmed, &pending, &otherHeld)
	if err != nil {
		return "", "", err
	}
	if f.Amount > p.Amount-confirmed-otherHeld {
		return "exception", "Provider refunds and confirmed merchant refunds exceed the captured amount.", nil
	}
	if f.Amount > p.Amount-confirmed-pending {
		if identity.count == 0 {
			_, err = tx.Exec(ctx, `INSERT INTO pay.refund_source_holds(source_operation,reference,payment_id,amount,currency,
			    charge_reference,original_payout,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
				f.OperationID, f.Reference, p.ID, f.Amount, f.Currency, f.ChargeReference, originalPayout, f.OccurredAt)
			if err != nil {
				return "", "", err
			}
		}
		return "pending", "Verified provider refund is held until the conflicting original merchant refund outcome is established.", tx.Commit(ctx)
	}
	id := newID("dr")
	_, err = tx.Exec(ctx, `INSERT INTO pay.refunds(id,payment_id,amount,currency,status,reference,source_operation,
		source_sequence,occurred_at,original_payout) VALUES($1,$2,$3,$4,'succeeded',$5,$6,nextval('pay.adjustment_sequence'),$7,$8)`,
		id, p.ID, f.Amount, f.Currency, f.Reference, f.OperationID, f.OccurredAt, originalPayout)
	if err != nil {
		return "", "", err
	}
	if err = note(ctx, tx, id, "provider_refund", "Provider-originated refund verified against the original capture and previously matched deposit."); err != nil {
		return "", "", err
	}
	if identity.count == 1 {
		if _, err = tx.Exec(ctx, "DELETE FROM pay.refund_source_holds WHERE source_operation=$1", f.OperationID); err != nil {
			return "", "", err
		}
	}
	return "applied", "Original provider refund verified. Awaiting independent evidence for its later remittance.", tx.Commit(ctx)
}

type Adjustment struct {
	Sequence        int64     `json:"sequence"`
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	MerchantID      string    `json:"merchant_id"`
	PaymentID       string    `json:"payment_id"`
	OrderID         string    `json:"order_id"`
	Charge          string    `json:"charge_reference"`
	Source          string    `json:"source_operation"`
	Reference       string    `json:"reference"`
	Amount          int64     `json:"amount"`
	Currency        string    `json:"currency"`
	OriginalPayout  string    `json:"original_payout"`
	OriginalBank    string    `json:"original_bank"`
	OriginalDeposit int64     `json:"original_deposit_minor"`
	OccurredAt      time.Time `json:"occurred_at"`
}

func (s *Service) listAdjustments(w http.ResponseWriter, r *http.Request) error {
	parameters := r.URL.Query()
	after, limit := int64(0), int64(50)
	for name, values := range parameters {
		if len(values) != 1 {
			return fail(400, "Supply one cursor and one page size.")
		}
		value, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || value < 0 || strconv.FormatInt(value, 10) != values[0] {
			return fail(400, "Invalid adjustment cursor or page size.")
		}
		switch name {
		case "after":
			after = value
		case "limit":
			if value < 1 || value > 100 {
				return fail(400, "Choose an adjustment page size between 1 and 100.")
			}
			limit = value
		default:
			return fail(400, "Unsupported adjustment query parameter.")
		}
	}
	rows, err := s.db.Query(r.Context(), `SELECT r.source_sequence,r.id,'provider_refund',p.merchant_id,p.id,p.order_id,
		p.reference,r.source_operation,r.reference,r.amount,r.currency,r.original_payout,b.bank_trace,b.net,r.occurred_at
		FROM pay.refunds r JOIN pay.payments p ON p.id=r.payment_id
		JOIN pay.payouts b ON b.id=r.original_payout AND b.status='matched' AND b.currency=p.currency
		WHERE p.merchant_id='mimi' AND r.source_sequence>$1 ORDER BY r.source_sequence LIMIT $2`, after, limit)
	if err != nil {
		return err
	}
	page, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Adjustment, error) {
		var a Adjustment
		err := row.Scan(&a.Sequence, &a.ID, &a.Kind, &a.MerchantID, &a.PaymentID, &a.OrderID, &a.Charge,
			&a.Source, &a.Reference, &a.Amount, &a.Currency, &a.OriginalPayout, &a.OriginalBank, &a.OriginalDeposit, &a.OccurredAt)
		return a, err
	})
	if err != nil {
		return err
	}
	if len(page) > 0 {
		after = page[len(page)-1].Sequence
	}
	answer(w, 200, map[string]any{"adjustments": page, "next_cursor": after})
	return nil
}

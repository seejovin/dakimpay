package simulator

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SourceRefund is the simulator's dated observation of an acquirer-originated
// refund. The payment, charge and refund identifiers remain stable on replay.
type SourceRefund struct {
	refund
	PaymentID  string    `json:"payment_id"`
	Origin     string    `json:"origin"`
	OccurredAt time.Time `json:"occurred_at"`
}

const movementFields = `SELECT r.operation_id,r.charge_reference,r.amount,r.merchant_id,r.currency,r.reference,
 c.operation_id,r.origin,r.created_at FROM sim_refund r JOIN sim_charge c ON r.charge_reference=c.reference
 WHERE r.merchant_id='mimi' AND c.merchant_id=r.merchant_id`

func readMovement(row pgx.Row) (SourceRefund, error) {
	var m SourceRefund
	err := row.Scan(&m.Operation, &m.Charge, &m.Amount, &m.Merchant, &m.Currency, &m.Reference,
		&m.PaymentID, &m.Origin, &m.OccurredAt)
	m.Status = "succeeded"
	m.OccurredAt = m.OccurredAt.UTC()
	return m, err
}

func (a *acquirer) getSourceRefund(w http.ResponseWriter, r *http.Request) {
	query := movementFields + " AND r.origin='acquirer'"
	key := r.PathValue("operation")
	if reference := r.PathValue("reference"); reference != "" {
		query += " AND r.reference=$1"
		key = reference
	} else {
		query += " AND r.operation_id=$1"
	}
	fact, err := readMovement(a.pool.QueryRow(r.Context(), query, key))
	if err != nil {
		respondError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, fact)
}

var sourceOperationID = regexp.MustCompile(`^sr_[a-zA-Z0-9._:-]{1,125}$`)

// RecordSourceRefund is a fixture entry point for the isolated acquirer. A
// merchant credential cannot invoke it. The originating capture must already
// belong to a frozen remittance before this scenario can introduce a later debit.
func RecordSourceRefund(ctx context.Context, pool *pgxpool.Pool, operationID, chargeReference, merchantID, currency string, amount int64) (SourceRefund, error) {
	if !sourceOperationID.MatchString(operationID) {
		return SourceRefund{}, invalid("Source refund requires a stable source operation ID")
	}
	return recordRefund(ctx, pool, refundRequest{Operation: operationID, Charge: chargeReference, Merchant: merchantID, Currency: currency, Amount: amount}, "acquirer")
}

// Both origins use the same serialized ledger. An insufficient-balance
// decision retains its terms across retries and produces no financial line.
func recordRefund(ctx context.Context, pool *pgxpool.Pool, req refundRequest, origin string) (SourceRefund, error) {
	if req.Merchant != merchant || req.Currency != currency || !validID(req.Operation) || !validID(req.Charge) || req.Amount <= 0 || req.Amount > limit {
		return SourceRefund{}, invalid("Invalid original payment or refund fields")
	}
	var result SourceRefund
	err := write(ctx, pool, func(tx pgx.Tx) error {
		row := func() pgx.Row {
			return tx.QueryRow(ctx, movementFields+" AND r.operation_id=$1", req.Operation)
		}
		prior, err := readMovement(row())
		if err == nil {
			if prior.refundRequest != req || prior.Origin != origin {
				return conflict("Refund operation already bound to another request")
			}
			result = prior
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		denied, err := readDecline(tx.QueryRow(ctx, declineFields, req.Operation))
		if err == nil {
			if denied.refundRequest != req || denied.Origin != origin {
				return conflict("Refund operation already bound to another request")
			}
			result = denied
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var status, order string
		var remaining int64
		var remitted bool
		err = tx.QueryRow(ctx, `SELECT status,order_id,
 c.amount-(SELECT coalesce(sum(amount),0) FROM sim_refund WHERE charge_reference=c.reference),
 EXISTS(SELECT FROM sim_line WHERE kind='capture' AND reference=c.reference AND payout_id IS NOT NULL)
 FROM sim_charge c WHERE c.reference=$1 AND c.merchant_id=$2 AND c.currency=$3 FOR UPDATE OF c`,
			req.Charge, req.Merchant, req.Currency).Scan(&status, &order, &remaining, &remitted)
		if err != nil {
			return err
		}
		if status != "captured" || (origin == "acquirer" && !remitted) {
			return conflict("The capture is not eligible for this refund")
		}
		if req.Amount > remaining {
			result.refund = refund{refundRequest: req, Status: "declined"}
			_, err = tx.Exec(ctx, `INSERT INTO sim_refund_decline(operation_id,charge_reference,amount,merchant_id,currency,origin)
 VALUES($1,$2,$3,$4,$5,$6)`, req.Operation, req.Charge, req.Amount, req.Merchant, req.Currency, origin)
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO sim_refund(operation_id,charge_reference,amount,merchant_id,currency,origin,created_at)
 VALUES($1,$2,$3,$4,$5,$6,clock_timestamp())`, req.Operation, req.Charge, req.Amount, req.Merchant, req.Currency, origin)
		if err != nil {
			return err
		}
		result, err = readMovement(row())
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO sim_line(kind,reference,amount,batch) VALUES('refund',$1,$2,$3)",
			result.Reference, -req.Amount, batchFor(order))
		if err != nil {
			return err
		}
		kind := "refund.succeeded"
		if origin == "acquirer" {
			kind = "refund.observed"
		}
		return enqueueCallback(ctx, tx, req.Merchant, req.Operation, kind)
	})
	if err != nil {
		return SourceRefund{}, err
	}
	if result.Status == "declined" {
		return SourceRefund{}, conflict("Refund exceeds remaining captured amount")
	}
	return result, nil
}

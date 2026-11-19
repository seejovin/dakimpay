package simulator

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

type feeEstimate struct {
	PaymentID  string    `json:"payment_id"`
	Reference  string    `json:"reference"`
	Amount     int64     `json:"amount"`
	Currency   string    `json:"currency"`
	Fee        int64     `json:"fee"`
	RateBPS    int       `json:"rate_bps"`
	Policy     string    `json:"policy"`
	RecordedAt time.Time `json:"recorded_at"`
}

type payoutFeeEvidence struct {
	Operation string        `json:"operation_id"`
	Merchant  string        `json:"merchant_id"`
	Currency  string        `json:"currency"`
	PayoutID  string        `json:"payout_id"`
	Trace     string        `json:"bank_trace"`
	Net       int64         `json:"net"`
	Lines     []line        `json:"lines"`
	Estimates []feeEstimate `json:"estimates"`
}

// Only the original settlement operation can identify this frozen payout.
// Reading it neither sweeps new lines nor sends another bank instruction.
func (a *acquirer) feeEvidence(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	operation := r.PathValue("operation")
	if !validID(operation) {
		respondError(w, invalid("Invalid settlement operation"))
		return
	}
	var evidence payoutFeeEvidence
	err := pgx.BeginTxFunc(ctx, a.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT operation_id,merchant_id,currency,id,bank_trace,net
			FROM sim_payout WHERE operation_id=$1 AND merchant_id=$2 AND currency=$3`, operation, merchant, currency).
			Scan(&evidence.Operation, &evidence.Merchant, &evidence.Currency, &evidence.PayoutID, &evidence.Trace, &evidence.Net)
		if err != nil {
			return err
		}
		evidence.Lines, err = payoutLines(ctx, tx, evidence.PayoutID)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT e.payment_id,e.reference,e.amount,e.currency,e.fee,e.rate_bps,e.policy,e.recorded_at
			FROM sim_line l JOIN sim_fee_estimate e ON e.reference=l.reference
			WHERE l.payout_id=$1 AND l.kind='capture' AND e.merchant_id=$2 AND e.currency=$3
			ORDER BY l.sequence`, evidence.PayoutID, merchant, currency)
		if err != nil {
			return err
		}
		evidence.Estimates, err = pgx.CollectRows(rows, pgx.RowToStructByPos[feeEstimate])
		return err
	})
	if err != nil {
		respondError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, evidence)
}

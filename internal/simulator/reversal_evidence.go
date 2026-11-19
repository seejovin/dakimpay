package simulator

import (
	"net/http"

	"dakimpay/internal/processor"
	"github.com/jackc/pgx/v5"
)

// One read-only snapshot binds the charge, its recorded dispute observation and
// every original refund. A second read of a moving page could lose a refund.
func (a *acquirer) reversalEvidence(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, err := a.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		respondError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	c, err := a.readCharge(tx.QueryRow(ctx, "SELECT "+chargeFields+" FROM sim_charge WHERE operation_id=$1 AND merchant_id=$2", r.PathValue("operation"), merchant))
	if err != nil {
		respondError(w, err)
		return
	}
	c.CheckoutURL = "" // Read-only evidence must not disclose a checkout capability.
	rows, err := tx.Query(ctx, `SELECT operation_id,charge_reference,amount,merchant_id,currency,reference
	 FROM sim_refund WHERE charge_reference=$1 ORDER BY operation_id LIMIT $2`, c.Reference, processor.MaxEvidenceRefunds+1)
	if err != nil {
		respondError(w, err)
		return
	}
	refunds, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (refund, error) { return readRefund(row) })
	if err != nil {
		respondError(w, err)
		return
	}
	if len(refunds) > processor.MaxEvidenceRefunds {
		respondError(w, conflict("Refund history exceeds the evidence limit. Keep the payment under review."))
		return
	}
	if err = tx.Commit(ctx); err != nil {
		respondError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"charge": c, "refunds": refunds, "complete": true, "dispute_status": c.DisputeStatus})
}

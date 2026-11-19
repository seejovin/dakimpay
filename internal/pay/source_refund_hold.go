package pay

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type sourceIdentity struct {
	count          int
	same           bool
	published      bool
	originalPayout string
}

// Claim a bounded number of original observations. Each lease is committed
// before the provider is queried. A missing notification or abandoned payout
// cannot strand a verified hold after its conflicting reservation resolves.
func (s *Service) recoverHeldRefunds(ctx context.Context) error {
	var problems error
	for range 4 {
		var operation, reference string
		var lease time.Time
		err := s.db.QueryRow(ctx, `WITH due AS (
		    SELECT source_operation FROM pay.refund_source_holds WHERE next_check<=now()
		    ORDER BY next_check,source_operation LIMIT 1 FOR UPDATE SKIP LOCKED)
		  UPDATE pay.refund_source_holds h SET next_check=clock_timestamp()+interval '45 seconds'
		  FROM due WHERE h.source_operation=due.source_operation
		  RETURNING h.source_operation,h.reference,h.next_check`).Scan(&operation, &reference, &lease)
		if errors.Is(err, pgx.ErrNoRows) {
			return problems
		}
		if err != nil {
			return errors.Join(problems, err)
		}
		status, detail, err := s.inspectSourceRefund(ctx, operation, reference)
		if err != nil {
			problems = errors.Join(problems, err)
			detail = "Original source refund could not be verified and stored. Retry scheduled."
		}
		if status == "applied" && err == nil {
			continue
		}
		// Update only the lease we claimed. Another worker might have
		// finished, or renewed this same hold while the provider was slow.
		_, err = s.db.Exec(ctx, `UPDATE pay.refund_source_holds SET last_message=$3,
		    next_check=clock_timestamp()+CASE WHEN $4 THEN interval '5 minutes' ELSE interval '45 seconds' END
		    WHERE source_operation=$1 AND next_check=$2`, operation, lease, detail, status == "exception")
		if err != nil {
			return errors.Join(problems, err)
		}
	}
	return problems
}

// The operation and upstream reference each identify one movement, whether
// parked or published. Combine both lookups so a mixed pair taken from two
// records cannot satisfy replay. The caller holds the capture and reference locks.
func lookupSourceIdentity(ctx context.Context, tx pgx.Tx, f sourceRefund) (sourceIdentity, error) {
	var identity sourceIdentity
	err := tx.QueryRow(ctx, `WITH candidates AS (
	    SELECT r.source_operation,r.reference,r.payment_id,r.amount,r.currency,p.reference AS charge_reference,
	      r.occurred_at,r.original_payout,r.status='succeeded' AS valid,true AS published
	    FROM pay.refunds r JOIN pay.payments p ON p.id=r.payment_id
	    WHERE r.source_operation=$1 OR r.reference=$2
	    UNION ALL
	    SELECT source_operation,reference,payment_id,amount,currency,charge_reference,occurred_at,original_payout,true,false
	    FROM pay.refund_source_holds WHERE source_operation=$1 OR reference=$2)
	  SELECT count(*),coalesce(bool_and(coalesce(source_operation=$1 AND reference=$2 AND payment_id=$3
	    AND amount=$4 AND currency=$5 AND charge_reference=$6 AND occurred_at=$7 AND valid,false)),true),
	    coalesce(bool_or(published),false),coalesce(min(original_payout),'') FROM candidates`,
		f.OperationID, f.Reference, f.PaymentID, f.Amount, f.Currency, f.ChargeReference, f.OccurredAt).
		Scan(&identity.count, &identity.same, &identity.published, &identity.originalPayout)
	return identity, err
}

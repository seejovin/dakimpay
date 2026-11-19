package pay

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// RunRecovery follows original pending operations without relying on the shopper
// returning to the merchant or the cashier's browser remaining open.
func (s *Service) RunRecovery(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := s.recoverDue(ctx); err != nil && ctx.Err() == nil {
			log.Printf("payment recovery: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Claim one payment and one refund per round. Persist the retry time before
// network I/O, so restarts and competing workers preserve the original operation.
const claimRecovery = `
WITH due_pay AS (
 SELECT id FROM pay.payments WHERE status IN ('unknown','awaiting_customer') AND next_recovery<=now()
 ORDER BY next_recovery,id LIMIT 1 FOR UPDATE SKIP LOCKED
), p AS (
 UPDATE pay.payments a SET recovery_attempts=least(a.recovery_attempts+1,6),
 next_recovery=now()+least(interval '30 minutes', interval '45 seconds' * (1 << least(a.recovery_attempts,6)))
 FROM due_pay d WHERE a.id=d.id RETURNING a.id
), due_ref AS (
 SELECT id FROM pay.refunds WHERE status='unknown' AND next_recovery<=now()
 ORDER BY next_recovery,id LIMIT 1 FOR UPDATE SKIP LOCKED
), f AS (
 UPDATE pay.refunds a SET recovery_attempts=least(a.recovery_attempts+1,6),
 next_recovery=now()+least(interval '30 minutes', interval '45 seconds' * (1 << least(a.recovery_attempts,6)))
 FROM due_ref d WHERE a.id=d.id RETURNING a.id
)
SELECT 'payment',id FROM p UNION ALL SELECT 'refund',id FROM f`

func (s *Service) recoverDue(ctx context.Context) error {
	type job struct{ kind, id string }
	for range 4 {
		rows, err := s.db.Query(ctx, claimRecovery)
		if err != nil {
			return err
		}
		jobs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (job, error) {
			var j job
			err := row.Scan(&j.kind, &j.id)
			return j, err
		})
		if err != nil {
			return err
		}
		if len(jobs) == 0 {
			return nil
		}
		for _, j := range jobs {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			switch j.kind {
			case "payment":
				var p Payment
				p, err = payment(ctx, s.db, j.id)
				if err == nil && p.Status != "captured" {
					_, err = s.syncCharge(ctx, p, true)
				}
			case "refund":
				var f Refund
				f, err = refundByID(ctx, s.db, j.id)
				if err == nil && f.Status != "succeeded" {
					_, err = s.syncRefund(ctx, f)
				}
			}
			if err != nil && ctx.Err() == nil {
				log.Printf("recover %s %s: %v", j.kind, j.id, err)
			}
		}
	}
	return nil
}

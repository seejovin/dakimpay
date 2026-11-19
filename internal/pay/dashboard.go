package pay

import (
	"context"
	"github.com/jackc/pgx/v5"
	"net/http"
	"time"
)

type Event struct {
	ID        int64     `json:"id"`
	Subject   string    `json:"subject"`
	Kind      string    `json:"kind"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}
type Totals struct {
	Captured       int64 `json:"captured"`
	Refunded       int64 `json:"refunded"`
	Fees           int64 `json:"fees"`
	Deposited      int64 `json:"deposited"`
	PendingDeposit int64 `json:"pending_deposit"`
}
type Dashboard struct {
	MerchantID string    `json:"merchant_id"`
	Currency   string    `json:"currency"`
	Payments   []Payment `json:"payments"`
	Refunds    []Refund  `json:"refunds"`
	Payouts    []Payout  `json:"payouts"`
	Events     []Event   `json:"events"`
	Totals     Totals    `json:"totals"`
}

func list[T any](ctx context.Context, db sqlDB, query string, scan func(pgx.Row) (T, error)) ([]T, error) {
	rows, err := db.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (T, error) { return scan(row) })
}
func (s *Service) dashboard(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	d := Dashboard{MerchantID: "mimi", Currency: "USD"}
	d.Payments, err = list(ctx, tx, "SELECT "+paymentColumns+" FROM pay.payments p WHERE p.merchant_id='mimi' ORDER BY created_at DESC,id DESC", readPayment)
	if err != nil {
		return err
	}
	d.Refunds, err = list(ctx, tx, "SELECT "+refundColumns+refundFrom+" ORDER BY r.created_at DESC,r.id DESC", readRefund)
	if err != nil {
		return err
	}
	d.Payouts, err = list(ctx, tx, "SELECT "+payoutColumns+" FROM pay.payouts b ORDER BY b.created_at DESC,b.id DESC", readPayout)
	if err != nil {
		return err
	}
	d.Events, err = list(ctx, tx, "SELECT id,subject,kind,message,created_at FROM pay.events ORDER BY id DESC", func(row pgx.Row) (Event, error) {
		var e Event
		err := row.Scan(&e.ID, &e.Subject, &e.Kind, &e.Message, &e.CreatedAt)
		return e, err
	})
	if err != nil {
		return err
	}
	for _, p := range d.Payments {
		if p.Status == "captured" {
			d.Totals.Captured += p.Amount
		}
	}
	for _, f := range d.Refunds {
		if f.Status == "succeeded" {
			d.Totals.Refunded += f.Amount
		}
	}
	for _, b := range d.Payouts {
		for _, l := range b.Lines {
			if l.Kind == "fee" {
				d.Totals.Fees -= l.Amount
			}
		}
		if b.Status == "matched" {
			d.Totals.Deposited += b.Net
		} else {
			d.Totals.PendingDeposit += b.Net
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	answer(w, 200, d)
	return nil
}

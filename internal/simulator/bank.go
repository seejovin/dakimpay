package simulator

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type bankInstruction struct {
	Trace    string `json:"trace"`
	Merchant string `json:"merchant_id"`
	Currency string `json:"currency"`
	Amount   int64  `json:"amount"`
}
type bankEntry struct {
	bankInstruction
	Status string `json:"status"`
}

func Bank(ctx context.Context, pool *pgxpool.Pool, token string) (http.Handler, error) {
	return BankWithKeys(ctx, pool, token, token, token)
}
func BankWithKeys(ctx context.Context, pool *pgxpool.Pool, readToken, instructionToken, postToken string) (http.Handler, error) {
	if readToken == "" || instructionToken == "" || postToken == "" {
		return nil, errors.New("bank requires service credentials")
	}
	if _, err := pool.Exec(ctx, bankSchema); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bank/health", health(pool))
	mux.HandleFunc("POST /bank/instructions", require(instructionToken, func(w http.ResponseWriter, r *http.Request) {
		var i bankInstruction
		if err := readJSON(r, &i); err != nil {
			respondError(w, err)
			return
		}
		if !validID(i.Trace) || i.Merchant != merchant || i.Currency != currency {
			respondError(w, invalid("Invalid bank instruction or beneficiary"))
			return
		}
		e := bankEntry{bankInstruction: i}
		ctx := r.Context()
		err := write(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "insert into sim_bank_entry(trace,merchant_id,currency,amount) values($1,$2,$3,$4) on conflict(trace) do nothing", i.Trace, i.Merchant, i.Currency, i.Amount)
			if err != nil {
				return err
			}
			err = tx.QueryRow(ctx, "select trace,merchant_id,currency,amount,case when posted_at is null then 'pending' else 'posted' end from sim_bank_entry where trace=$1", i.Trace).Scan(&e.Trace, &e.Merchant, &e.Currency, &e.Amount, &e.Status)
			if err == nil && e.bankInstruction != i {
				return conflict("Bank reference already bound to different instruction")
			}
			return err
		})
		if err != nil {
			respondError(w, err)
			return
		}
		jsonResponse(w, 200, e)
	}))
	mux.HandleFunc("POST /bank/post/{trace}", require(postToken, func(w http.ResponseWriter, r *http.Request) {
		var e bankEntry
		err := pool.QueryRow(r.Context(), `update sim_bank_entry set posted_at=coalesce(posted_at,now()) where trace=$1 returning trace,merchant_id,currency,amount`, r.PathValue("trace")).Scan(&e.Trace, &e.Merchant, &e.Currency, &e.Amount)
		if err != nil {
			respondError(w, err)
			return
		}
		e.Status = "posted"
		jsonResponse(w, 200, e)
	}))
	mux.HandleFunc("GET /bank/entries/{trace}", require(readToken, func(w http.ResponseWriter, r *http.Request) {
		var e bankEntry
		err := pool.QueryRow(r.Context(), "select trace,merchant_id,currency,amount from sim_bank_entry where trace=$1 and posted_at is not null", r.PathValue("trace")).Scan(&e.Trace, &e.Merchant, &e.Currency, &e.Amount)
		if err != nil {
			respondError(w, err)
			return
		}
		e.Status = "posted"
		jsonResponse(w, 200, e)
	}))
	return headers(mux), nil
}

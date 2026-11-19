package simulator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"
)

type refundRequest struct {
	Operation string `json:"operation_id"`
	Charge    string `json:"charge_reference"`
	Amount    int64  `json:"amount"`
	Merchant  string `json:"merchant_id"`
	Currency  string `json:"currency"`
}
type refund struct {
	refundRequest
	Reference string `json:"reference"`
	Status    string `json:"status"`
}

func (a *acquirer) createRefund(w http.ResponseWriter, r *http.Request) {
	var req refundRequest
	if err := readJSON(r, &req); err != nil {
		respondError(w, err)
		return
	}
	if req.Merchant != merchant || req.Currency != currency || !validID(req.Operation) || !validID(req.Charge) || req.Amount <= 0 || req.Amount > limit {
		respondError(w, invalid("Invalid original payment or refund fields"))
		return
	}
	ctx := r.Context()
	result := refund{refundRequest: req, Status: "succeeded"}
	err := write(ctx, a.pool, func(tx pgx.Tx) error {
		var prior refundRequest
		err := tx.QueryRow(ctx, "select operation_id,charge_reference,amount,merchant_id,currency,reference from sim_refund where operation_id=$1", req.Operation).Scan(&prior.Operation, &prior.Charge, &prior.Amount, &prior.Merchant, &prior.Currency, &result.Reference)
		if err == nil {
			if prior != req {
				return conflict("Refund operation already bound to another request")
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var status, order string
		var available int64
		err = tx.QueryRow(ctx, `select status,order_id,amount-(select coalesce(sum(amount),0) from sim_refund where charge_reference=c.reference)
			from sim_charge c where reference=$1 and merchant_id=$2 and currency=$3`, req.Charge, req.Merchant, req.Currency).Scan(&status, &order, &available)
		if err != nil {
			return err
		}
		if status != "captured" || req.Amount > available {
			return conflict("Refund exceeds remaining captured amount")
		}
		err = tx.QueryRow(ctx, `insert into sim_refund(operation_id,charge_reference,amount,merchant_id,currency) values($1,$2,$3,$4,$5) returning reference`,
			req.Operation, req.Charge, req.Amount, req.Merchant, req.Currency).Scan(&result.Reference)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "insert into sim_line(kind,reference,amount,batch) values('refund',$1,$2,$3)", result.Reference, -req.Amount, batchFor(order))
		return err
	})
	if err != nil {
		respondError(w, err)
		return
	}
	jsonResponse(w, 200, result)
}

type payoutRequest struct {
	Operation string `json:"operation_id"`
	Merchant  string `json:"merchant_id"`
	Currency  string `json:"currency"`
	Batch     string `json:"batch"`
}
type line struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Amount    int64  `json:"amount"`
}
type payout struct {
	ID       string `json:"id"`
	Trace    string `json:"bank_trace"`
	Merchant string `json:"merchant_id"`
	Currency string `json:"currency"`
	Net      int64  `json:"net"`
	Lines    []line `json:"lines"`
}

func (a *acquirer) settle(w http.ResponseWriter, r *http.Request) {
	var req payoutRequest
	if err := readJSON(r, &req); err != nil {
		respondError(w, err)
		return
	}
	if req.Batch == "" {
		req.Batch = "standard"
	}
	if req.Merchant != merchant || req.Currency != currency || !validID(req.Operation) || (req.Batch != "standard" && req.Batch != "guided") {
		respondError(w, invalid("Invalid payout scope"))
		return
	}
	var p payout
	ctx := r.Context()
	err := write(ctx, a.pool, func(tx pgx.Tx) error {
		var batch string
		err := tx.QueryRow(ctx, "select id,bank_trace,merchant_id,currency,net,batch from sim_payout where operation_id=$1", req.Operation).Scan(&p.ID, &p.Trace, &p.Merchant, &p.Currency, &p.Net, &batch)
		switch {
		case err == nil:
			if batch != req.Batch || p.Merchant != req.Merchant || p.Currency != req.Currency {
				return conflict("Payout key already used for another scope")
			}
		case errors.Is(err, pgx.ErrNoRows):
			var n int
			err = tx.QueryRow(ctx, "select count(*),coalesce(sum(amount),0) from sim_line where payout_id is null and batch=$1", req.Batch).Scan(&n, &p.Net)
			if err != nil {
				return err
			}
			if n == 0 {
				return conflict("No new statement movements for this payout")
			}
			p.Merchant, p.Currency = req.Merchant, req.Currency
			err = tx.QueryRow(ctx, `insert into sim_payout(operation_id,merchant_id,currency,batch,net) values($1,$2,$3,$4,$5) returning id,bank_trace`,
				req.Operation, req.Merchant, req.Currency, req.Batch, p.Net).Scan(&p.ID, &p.Trace)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, "update sim_line set payout_id=$1 where payout_id is null and batch=$2", p.ID, req.Batch); err != nil {
				return err
			}
		default:
			return err
		}
		rows, err := tx.Query(ctx, "select id,kind,reference,amount from sim_line where payout_id=$1 order by sequence", p.ID)
		if err != nil {
			return err
		}
		p.Lines, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (line, error) {
			var l line
			err := row.Scan(&l.ID, &l.Kind, &l.Reference, &l.Amount)
			return l, err
		})
		return err
	})
	if err != nil {
		respondError(w, err)
		return
	}
	if err := a.instructBank(ctx, p); err != nil {
		respondError(w, failure{503, "Statement frozen; retry the same payout operation to deliver its bank instruction"})
		return
	}
	jsonResponse(w, 200, p)
}

func (a *acquirer) instructBank(ctx context.Context, p payout) error {
	raw, err := json.Marshal(bankInstruction{p.Trace, p.Merchant, p.Currency, p.Net})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", a.bankURL+"/bank/instructions", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.bankInstructionToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return errors.New("bank instruction rejected")
	}
	var ack bankEntry
	if err = json.NewDecoder(io.LimitReader(res.Body, 16384)).Decode(&ack); err != nil {
		return err
	}
	if ack.Trace != p.Trace || ack.Merchant != p.Merchant || ack.Currency != p.Currency || ack.Amount != p.Net || (ack.Status != "pending" && ack.Status != "posted") {
		return errors.New("bank acknowledgment mismatch")
	}
	return nil
}

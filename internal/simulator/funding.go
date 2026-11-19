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

func readRefund(row pgx.Row) (refund, error) {
	r := refund{Status: "succeeded"}
	err := row.Scan(&r.Operation, &r.Charge, &r.Amount, &r.Merchant, &r.Currency, &r.Reference)
	return r, err
}

const declineFields = "SELECT operation_id,charge_reference,amount,merchant_id,currency,origin FROM sim_refund_decline WHERE operation_id=$1 AND merchant_id='mimi'"

func readDecline(row pgx.Row) (SourceRefund, error) {
	var f SourceRefund
	f.Status = "declined"
	err := row.Scan(&f.Operation, &f.Charge, &f.Amount, &f.Merchant, &f.Currency, &f.Origin)
	return f, err
}

func (a *acquirer) getRefund(w http.ResponseWriter, r *http.Request) {
	operation := r.PathValue("operation")
	result, err := readRefund(a.pool.QueryRow(r.Context(), `SELECT operation_id,charge_reference,amount,merchant_id,currency,reference
 FROM sim_refund WHERE operation_id=$1 AND merchant_id='mimi' AND origin='merchant'`, operation))
	if errors.Is(err, pgx.ErrNoRows) {
		var declined SourceRefund
		declined, err = readDecline(a.pool.QueryRow(r.Context(), declineFields+" AND origin='merchant'", operation))
		result = declined.refund
	}
	if err != nil {
		respondError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, result)
}

func (a *acquirer) createRefund(w http.ResponseWriter, r *http.Request) {
	var req refundRequest
	if err := readJSON(r, &req); err != nil {
		respondError(w, err)
		return
	}
	result, err := recordRefund(r.Context(), a.pool, req, "merchant")
	if err != nil {
		respondError(w, err)
		return
	}
	jsonResponse(w, 200, result.refund)
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

func payoutLines(ctx context.Context, tx pgx.Tx, id string) ([]line, error) {
	rows, err := tx.Query(ctx, "SELECT id,kind,reference,amount FROM sim_line WHERE payout_id=$1 ORDER BY sequence", id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[line])
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
		p.Lines, err = payoutLines(ctx, tx, p.ID)
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

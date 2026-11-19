package simulator

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type acquirer struct {
	pool                                     *pgxpool.Pool
	publicURL, bankURL, bankInstructionToken string
	client                                   *http.Client
}

func Acquirer(ctx context.Context, pool *pgxpool.Pool, publicURL, bankURL, token string) (http.Handler, error) {
	return AcquirerWithKeys(ctx, pool, publicURL, bankURL, token, token)
}

// Use distinct keys for processing, synthetic customer approvals and the bank
// instruction channel. The ordinary merchant-facing app gets the processing key.
func AcquirerWithKeys(ctx context.Context, pool *pgxpool.Pool, publicURL, bankURL, serviceToken, instructionToken string, approveTokens ...string) (http.Handler, error) {
	if serviceToken == "" || instructionToken == "" {
		return nil, errors.New("acquirer requires service credentials")
	}
	approveToken := serviceToken
	if len(approveTokens) > 0 {
		approveToken = approveTokens[0]
	}
	if approveToken == "" || len(approveTokens) > 1 {
		return nil, errors.New("invalid simulated approval credential")
	}
	for _, addr := range []string{publicURL, bankURL} {
		u, err := url.Parse(addr)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("invalid simulator base URL")
		}
	}
	if _, err := pool.Exec(ctx, acquirerSchema); err != nil {
		return nil, err
	}
	a := &acquirer{pool: pool, publicURL: strings.TrimRight(publicURL, "/"), bankURL: strings.TrimRight(bankURL, "/"), bankInstructionToken: instructionToken, client: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sim/health", health(pool))
	mux.HandleFunc("POST /sim/charges", require(serviceToken, a.createCharge))
	mux.HandleFunc("GET /sim/charges/{operation}", require(serviceToken, a.getCharge))
	mux.HandleFunc("POST /sim/approve/{operation}", require(approveToken, a.simulateApproval))
	mux.HandleFunc("GET /checkout/{capability}", a.checkout)
	mux.HandleFunc("POST /checkout/{capability}", a.checkout)
	mux.HandleFunc("POST /sim/refunds", require(serviceToken, a.createRefund))
	mux.HandleFunc("POST /sim/payouts", require(serviceToken, a.settle))
	return headers(mux), nil
}

type chargeRequest struct {
	Operation    string `json:"operation_id"`
	Merchant     string `json:"merchant_id"`
	Order        string `json:"order_id"`
	Channel      string `json:"channel"`
	Amount       int64  `json:"amount"`
	Currency     string `json:"currency"`
	Brand        string `json:"brand"`
	Terminal     string `json:"terminal_id"`
	LoseResponse bool   `json:"lose_response"`
}

type charge struct {
	Operation   string `json:"operation_id"`
	Reference   string `json:"reference"`
	Status      string `json:"status"`
	Merchant    string `json:"merchant_id"`
	Order       string `json:"order_id"`
	Channel     string `json:"channel"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	Brand       string `json:"brand"`
	Terminal    string `json:"terminal_id"`
	CheckoutURL string `json:"checkout_url"`
}

const chargeFields = "operation_id,reference,status,merchant_id,order_id,channel,amount,currency,brand,terminal_id,coalesce(checkout_token,'')"

func (a *acquirer) readCharge(row pgx.Row) (charge, error) {
	var c charge
	var cap string
	err := row.Scan(&c.Operation, &c.Reference, &c.Status, &c.Merchant, &c.Order, &c.Channel, &c.Amount, &c.Currency, &c.Brand, &c.Terminal, &cap)
	if cap != "" {
		c.CheckoutURL = a.publicURL + "/checkout/" + cap
	}
	return c, err
}

func (c chargeRequest) validate() error {
	if !validID(c.Operation) || !validID(c.Order) || c.Merchant != merchant || c.Currency != currency || c.Amount <= 0 || c.Amount > limit {
		return invalid("Invalid merchant, order, currency or amount")
	}
	switch c.Brand {
	case "visa", "mastercard", "amex":
	default:
		return invalid("Select a supported simulated card brand")
	}
	switch c.Channel {
	case "terminal":
		if c.Terminal != "MIMI-01" {
			return invalid("Terminal is not assigned to Mimi")
		}
	case "online":
		if c.Terminal != "" || c.LoseResponse {
			return invalid("Online checkout does not use a terminal or terminal interruption")
		}
	default:
		return invalid("Select terminal or online")
	}
	return nil
}

func (r chargeRequest) matches(c charge) bool {
	return r.Operation == c.Operation && r.Order == c.Order && r.Merchant == c.Merchant && r.Channel == c.Channel && r.Amount == c.Amount && r.Currency == c.Currency && r.Brand == c.Brand && r.Terminal == c.Terminal
}
func batchFor(order string) string {
	if order == "MIMI-GUIDED-TERMINAL" || order == "MIMI-GUIDED-ONLINE" {
		return "guided"
	}
	return "standard"
}

func recordCapture(ctx context.Context, tx pgx.Tx, c charge) error {
	rate := int64(200)
	if c.Channel == "online" {
		rate = 250
	}
	fee := (c.Amount*rate + 5000) / 10000
	_, err := tx.Exec(ctx, `insert into sim_line(kind,reference,amount,batch) values('capture',$1,$2,$3),('fee',$1,$4,$3)`, c.Reference, c.Amount, batchFor(c.Order), -fee)
	return err
}

func (a *acquirer) createCharge(w http.ResponseWriter, r *http.Request) {
	var req chargeRequest
	if err := readJSON(r, &req); err != nil {
		respondError(w, err)
		return
	}
	if err := req.validate(); err != nil {
		respondError(w, err)
		return
	}
	ctx := r.Context()
	var c charge
	dropped := false
	err := write(ctx, a.pool, func(tx pgx.Tx) error {
		prior, err := a.readCharge(tx.QueryRow(ctx, "select "+chargeFields+" from sim_charge where operation_id=$1 or (merchant_id=$2 and order_id=$3) order by (operation_id=$1) desc limit 1", req.Operation, req.Merchant, req.Order))
		if err == nil {
			if !req.matches(prior) {
				return conflict("Order or operation already bound to another request")
			}
			c = prior
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var cap *string
		status := "captured"
		if req.Channel == "online" {
			key, err := tokenID()
			if err != nil {
				return err
			}
			cap = &key
			status = "awaiting_customer"
		}
		c, err = a.readCharge(tx.QueryRow(ctx, `insert into sim_charge(operation_id,merchant_id,order_id,channel,amount,currency,brand,terminal_id,status,checkout_token,captured_at)
			values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,case when $9='captured' then now() end) returning `+chargeFields, req.Operation, req.Merchant, req.Order, req.Channel, req.Amount, req.Currency, req.Brand, req.Terminal, status, cap))
		if err != nil {
			return err
		}
		if c.Status == "captured" {
			dropped = req.LoseResponse
			return recordCapture(ctx, tx, c)
		}
		return nil
	})
	if err != nil {
		respondError(w, err)
		return
	}
	if dropped {
		// The charge and its statement lines already committed independently.
		// net/http closes HTTP/1 or resets HTTP/2 without sending a response.
		panic(http.ErrAbortHandler)
	}
	jsonResponse(w, 200, c)
}

func (a *acquirer) getCharge(w http.ResponseWriter, r *http.Request) {
	c, err := a.readCharge(a.pool.QueryRow(r.Context(), "select "+chargeFields+" from sim_charge where operation_id=$1", r.PathValue("operation")))
	if err != nil {
		respondError(w, err)
		return
	}
	jsonResponse(w, 200, c)
}

func (a *acquirer) approve(ctx context.Context, op, cap string) (charge, error) {
	var c charge
	err := write(ctx, a.pool, func(tx pgx.Tx) error {
		var err error
		c, err = a.readCharge(tx.QueryRow(ctx, "select "+chargeFields+" from sim_charge where (operation_id=$1 or checkout_token=$2) and channel='online'", op, cap))
		if err != nil {
			return err
		}
		if c.Status == "captured" {
			return nil
		}
		_, err = tx.Exec(ctx, "update sim_charge set status='captured',captured_at=now() where operation_id=$1", c.Operation)
		if err != nil {
			return err
		}
		c.Status = "captured"
		return recordCapture(ctx, tx, c)
	})
	return c, err
}
func (a *acquirer) simulateApproval(w http.ResponseWriter, r *http.Request) {
	c, err := a.approve(r.Context(), r.PathValue("operation"), "")
	if err != nil {
		respondError(w, err)
		return
	}
	jsonResponse(w, 200, c)
}

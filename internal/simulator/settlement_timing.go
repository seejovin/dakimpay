package simulator

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type payoutTimingRecord struct {
	Operation string    `json:"operation_id"`
	Payout    string    `json:"payout_id"`
	Trace     string    `json:"bank_trace"`
	Merchant  string    `json:"merchant_id"`
	Currency  string    `json:"currency"`
	Amount    int64     `json:"amount"`
	CreatedAt time.Time `json:"created_at"`
}

type bankTimingRecord struct {
	Trace    string    `json:"bank_trace"`
	Merchant string    `json:"merchant_id"`
	Currency string    `json:"currency"`
	Amount   int64     `json:"amount"`
	PostedAt time.Time `json:"posted_at"`
}

type timingInventory[T any] struct {
	Merchant string    `json:"merchant_id"`
	Currency string    `json:"currency"`
	AsOf     time.Time `json:"as_of"`
	Complete bool      `json:"complete"`
	Records  []T       `json:"records"`
}

const payoutTimingQuery = `SELECT operation_id,id,bank_trace,merchant_id,currency,net,created_at
	FROM sim_payout WHERE merchant_id=$1 AND currency=$2 AND created_at<=$3
	ORDER BY created_at,id`

const bankTimingQuery = `SELECT trace,merchant_id,currency,amount,posted_at
	FROM sim_bank_entry WHERE merchant_id=$1 AND currency=$2 AND posted_at<=$3
	ORDER BY posted_at,trace`

// One SELECT observes one source snapshot. A short inventory could misclassify
// another institution's entry as missing, so send either every row or an error.
func settlementTiming[T any](db *pgxpool.Pool, query string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(params) != 1 || len(params["as_of"]) != 1 {
			respondError(w, invalid("Supply exactly one as_of timestamp with a time zone."))
			return
		}
		cutoff, err := time.Parse(time.RFC3339Nano, params.Get("as_of"))
		if err != nil || cutoff.IsZero() {
			respondError(w, invalid("Invalid as_of timestamp. Use RFC 3339 with a time zone."))
			return
		}
		inventory := timingInventory[T]{Merchant: merchant, Currency: currency, AsOf: cutoff.UTC(), Complete: true, Records: []T{}}
		header, err := json.Marshal(inventory)
		if err != nil {
			respondError(w, err)
			return
		}
		size := len(header) + 1 // Include the encoder's trailing newline.
		rows, err := db.Query(r.Context(), query, merchant, currency, cutoff)
		if err != nil {
			respondError(w, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			row, err := pgx.RowToStructByPos[T](rows)
			if err != nil {
				respondError(w, err)
				return
			}
			wire, err := json.Marshal(row)
			if err != nil {
				respondError(w, err)
				return
			}
			size += len(wire)
			if len(inventory.Records) > 0 {
				size++ // comma between array elements
			}
			if size > 4<<20 { // Maximum accepted connector response.
				respondError(w, failure{http.StatusServiceUnavailable, "Complete timing inventory exceeds the connector response limit."})
				return
			}
			inventory.Records = append(inventory.Records, row)
		}
		if err := rows.Err(); err != nil {
			respondError(w, err)
			return
		}
		jsonResponse(w, http.StatusOK, inventory)
	}
}

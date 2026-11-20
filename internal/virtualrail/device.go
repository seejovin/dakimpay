package virtualrail

import (
	"crypto/sha256"
	"errors"
	"net/http"

	"dakimpay/internal/railcontract"
	"github.com/jackc/pgx/v5"
)

type deviceCommand struct {
	Version  int                `json:"version"`
	Reader   string             `json:"terminal_id"`
	Merchant string             `json:"merchant_id"`
	Outlet   string             `json:"outlet_id"`
	Command  *railcontract.Fact `json:"command"`
}

func (s *service) deviceHTTP(w http.ResponseWriter, r *http.Request) {
	e, ok := s.credentials[sha256.Sum256([]byte(bearer(r.Header)))]
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "Reader credential required."})
		return
	}
	scope := `merchant=$1 AND account=$2 AND outlet=$3 AND terminal=$4 AND channel='terminal' AND kind='charge'`
	args := []any{e.Merchant, e.Account, e.Outlet, e.ID}
	if r.Method == http.MethodGet {
		var id string
		err := s.db.QueryRow(r.Context(), "SELECT id FROM virtualrail.operation WHERE "+scope+" AND state='pending'", args...).Scan(&id)
		response := deviceCommand{Version: 1, Reader: e.ID, Merchant: e.Merchant, Outlet: e.Outlet}
		if err == nil {
			f, err := s.observe(r.Context(), scope+" AND id=$5", append(args, id), "", "")
			if err != nil {
				writeFailure(w, err)
				return
			}
			if f.State == "pending" {
				response.Command = &f
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			writeFailure(w, err)
			return
		}
		writeJSON(w, 200, response)
		return
	}
	var in struct {
		Decision  string `json:"decision"`
		CardBrand string `json:"card_brand,omitempty"`
	}
	if err := readBody(r, &in); err != nil {
		writeFailure(w, err)
		return
	}
	if (in.Decision != "approve" && in.Decision != "decline") || (in.Decision == "decline" && in.CardBrand != "") {
		writeFailure(w, reject(422, "Select approve or decline."))
		return
	}
	f, err := s.observe(r.Context(), scope+" AND id=$5", append(args, r.PathValue("id")), in.Decision, in.CardBrand)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, 200, f)
}

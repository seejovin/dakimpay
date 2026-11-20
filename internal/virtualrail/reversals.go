package virtualrail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"dakimpay/internal/railcontract"
	"github.com/jackc/pgx/v5"
)

// The virtual processor's supported movements are original charges and
// referenced merchant refunds only. "none" describes that synthetic dispute
// inventory. Real connectors must qualify their own coverage and freshness.
func (s *service) releaseHTTP(w http.ResponseWriter, r *http.Request) {
	m, a, id := r.PathValue("merchant"), r.PathValue("account"), r.PathValue("id")
	program, ok := s.programs[a]
	if !ok || program.Merchant != m || !validID.MatchString(id) {
		writeFailure(w, reject(404, "Original charge not found."))
		return
	}
	ctx := r.Context()
	inv := railcontract.ReleaseInventory{DisputeStatus: "none", Refunds: []railcontract.Fact{}}
	err := s.ledgerTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT statement_timestamp()").Scan(&inv.ObservedAt); err != nil {
			return err
		}
		f, err := s.scan(tx.QueryRow(ctx, "SELECT "+fields+" FROM virtualrail.operation WHERE "+original+" AND kind='charge'", m, a, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return reject(404, "Original charge not found.")
		}
		if err != nil {
			return err
		}
		inv.Fact = f
		rows, err := tx.Query(ctx, "SELECT "+fields+" FROM virtualrail.operation WHERE merchant=$1 AND account=$2 AND parent=$3 AND kind='refund' ORDER BY id LIMIT 251", m, a, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if len(inv.Refunds) == 250 {
				return nil
			}
			f, err := s.scan(rows)
			if err != nil {
				return err
			}
			inv.Refunds = append(inv.Refunds, f)
		}
		inv.Complete = rows.Err() == nil
		return rows.Err()
	})
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, 200, inv)
}

func (c *Client) InspectRelease(ctx context.Context, cmd railcontract.Command) (railcontract.ReleaseInventory, error) {
	var inv railcontract.ReleaseInventory
	route := "/rail/v1/accounts/" + url.PathEscape(cmd.Account) + "/merchants/" + url.PathEscape(cmd.Merchant) + "/operations/" + url.PathEscape(cmd.ID) + "/release-evidence"
	code, raw, err := c.call(ctx, http.MethodGet, route, nil)
	if err != nil {
		return inv, err
	}
	if code != 200 {
		return inv, fmt.Errorf("processor inventory unavailable: HTTP %d", code)
	}
	if decodeEvidence(raw, &inv) != nil || inv.ObservedAt.IsZero() || inv.Refunds == nil || cmd.Kind != "charge" || c.validateFact(inv.Fact, cmd) != nil {
		return railcontract.ReleaseInventory{}, railcontract.ErrEvidenceConflict
	}
	seen := map[string]bool{}
	for _, f := range inv.Refunds {
		if !validID.MatchString(f.ID) || f.ID == cmd.ID || seen[f.ID] || f.Amount <= 0 {
			return railcontract.ReleaseInventory{}, railcontract.ErrEvidenceConflict
		}
		expected := cmd
		expected.ID = f.ID
		expected.Amount = f.Amount
		expected.Kind = "refund"
		expected.Parent = cmd.ID
		if c.validateFact(f, expected) != nil || (f.State == "refunded" && f.CardBrand != inv.Fact.CardBrand) {
			return railcontract.ReleaseInventory{}, railcontract.ErrEvidenceConflict
		}
		seen[f.ID] = true
	}
	return inv, nil
}

var _ railcontract.ReleaseInspector = (*Client)(nil)

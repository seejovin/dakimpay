package pay

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"

	"dakimpay/internal/processor"
	"github.com/jackc/pgx/v5"
)

type acquirerReversals struct {
	Charge        chargeFact   `json:"charge"`
	Refunds       []refundFact `json:"refunds"`
	Complete      bool         `json:"complete"`
	DisputeStatus string       `json:"dispute_status"`
}

var evidenceKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// The connector contract uses exact lowercase field names. Go's default
// decoding accepts duplicate fields and case aliases, either of which could
// conceal an earlier dispute or replace a refund amount in the same message.
func (a *acquirerReversals) UnmarshalJSON(raw []byte) error {
	type plain acquirerReversals
	return decodeEvidence(raw, (*plain)(a))
}

func decodeEvidence(raw []byte, out any) error {
	return decodeBoundedEvidence(raw, out, processor.MaxEvidenceRefunds)
}

func decodeBoundedEvidence(raw []byte, out any, maxItems int) error {
	wire := json.NewDecoder(bytes.NewReader(raw))
	wire.UseNumber()
	if err := checkEvidenceValue(wire, 0, maxItems); err != nil {
		return err
	}
	typed := json.NewDecoder(bytes.NewReader(raw))
	typed.DisallowUnknownFields()
	return typed.Decode(out)
}

func checkEvidenceValue(d *json.Decoder, depth, maxItems int) error {
	if depth > 3 {
		return errors.New("unexpected nesting in reversal evidence")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		seen := make(map[string]bool)
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || len(seen) >= 16 || seen[name] || !evidenceKey.MatchString(name) {
				return errors.New("invalid or repeated reversal evidence field")
			}
			seen[name] = true
			if err = checkEvidenceValue(d, depth+1, maxItems); err != nil {
				return err
			}
		}
	case json.Delim('['):
		for i := 0; d.More(); i++ {
			if i >= maxItems {
				return errors.New("evidence array exceeds its item limit")
			}
			if err = checkEvidenceValue(d, depth+1, maxItems); err != nil {
				return err
			}
		}
	default:
		return nil
	}
	_, err = d.Token()
	return err
}

type refundEvidence struct {
	Refund
	ChargeReference string `json:"charge_reference"`
}

type merchantReversals struct {
	Payment       Payment          `json:"payment"`
	Refunds       []refundEvidence `json:"refunds"`
	DisputeStatus string           `json:"dispute_status"`
	Complete      bool             `json:"complete"`
}

func (s *Service) reversalEvidence(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	original, err := payment(ctx, s.db, r.PathValue("id"))
	if err != nil {
		return err
	}
	if original.Status != "captured" || original.Reference == "" {
		return fail(409, "Original capture has not been verified.")
	}
	var source acquirerReversals
	path := "/sim/charges/" + url.PathEscape(original.ID) + "/reversal-evidence"
	code, err := s.remote(ctx, s.acquirer, http.MethodGet, path, nil, &source)
	if err != nil {
		if code == http.StatusConflict {
			return fail(409, "The acquirer cannot supply a complete refund history.")
		}
		return fail(502, "Current acquirer evidence is unavailable. Keep the payment under review.")
	}

	// Take the merchant snapshot after the source lookup. Any local reservation
	// committed while the lookup was in flight must participate in the check.
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	p, err := payment(ctx, tx, original.ID)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, "SELECT "+refundColumns+refundFrom+
		" AND r.payment_id=$1 AND r.status<>'declined' ORDER BY r.id LIMIT $2", p.ID, processor.MaxEvidenceRefunds+1)
	if err != nil {
		return err
	}
	refunds, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Refund, error) { return readRefund(row) })
	if err != nil {
		return err
	}
	proof, err := matchReversals(p, refunds, source)
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	answer(w, http.StatusOK, proof)
	return nil
}

func matchReversals(p Payment, refunds []Refund, source acquirerReversals) (merchantReversals, error) {
	var empty merchantReversals
	if p.RefundBalanceReview || !source.Complete || source.Refunds == nil || len(source.Refunds) > processor.MaxEvidenceRefunds ||
		p.Status != "captured" || source.Charge.Status != "captured" ||
		!source.Charge.boundTo(p) || source.Charge.Reference != p.Reference {
		return empty, fail(409, "Current acquirer evidence is incomplete or inconsistent with the original capture.")
	}
	switch source.DisputeStatus {
	case "none", "open", "unknown":
	default:
		return empty, fail(409, "Missing or unrecognized dispute observation.")
	}
	if len(refunds) != len(source.Refunds) {
		return empty, fail(409, "Local refund reservations and the acquirer refund history differ.")
	}
	byID := make(map[string]refundFact, len(source.Refunds))
	for _, fact := range source.Refunds {
		if _, duplicate := byID[fact.OperationID]; duplicate {
			return empty, fail(409, "Duplicate refund operation in the acquirer evidence.")
		}
		byID[fact.OperationID] = fact
	}
	result := merchantReversals{Payment: p, Refunds: make([]refundEvidence, 0, len(refunds)), DisputeStatus: source.DisputeStatus, Complete: true}
	var total int64
	for _, f := range refunds {
		fact, ok := byID[originalRefund(f, p).OperationID]
		if !ok || f.PaymentID != p.ID || f.MerchantID != p.MerchantID || f.Currency != p.Currency ||
			f.Status != "succeeded" || fact.Status != "succeeded" ||
			fact.refundRequest != originalRefund(f, p) || f.Reference != fact.Reference ||
			!identifier.MatchString(f.Reference) || f.Amount <= 0 || f.Amount > p.Amount-total {
			return empty, fail(409, "A refund is pending or its terms differ from the original operation.")
		}
		total += f.Amount
		result.Refunds = append(result.Refunds, refundEvidence{Refund: f, ChargeReference: p.Reference})
	}
	if total != p.Refunded {
		return empty, fail(409, "The itemized refunds do not match the captured payment's refund total.")
	}
	return result, nil
}

package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
)

// This is the digest of the normalized processor evidence accepted at the
// connector interface. The record never publishes an access-bearing checkout
// URL or terms the source incorrectly attributed to another merchant.
type engineEvidence struct {
	PaymentMethod    string    `json:"payment_method,omitempty"`
	RouteID          string    `json:"route_id,omitempty"`
	ConnectorID      string    `json:"connector_id,omitempty"`
	ProviderAccount  string    `json:"provider_account,omitempty"`
	Ledger           string    `json:"processor_ledger,omitempty"`
	CardBrand        string    `json:"card_brand,omitempty"`
	Type             string    `json:"type"`
	Decision         string    `json:"decision"`
	Reason           string    `json:"reason,omitempty"`
	Reference        string    `json:"movement_reference,omitempty"`
	Actor            string    `json:"actor,omitempty"`
	Role             Role      `json:"actor_role,omitempty"`
	GrantDigest      string    `json:"grant_sha256,omitempty"`
	NormalizedDigest string    `json:"normalized_sha256,omitempty"`
	At               time.Time `json:"recorded_at"`
}

func engineEvidencePrefix(c railcontract.Command) string {
	return engineKey("evidence", c.Merchant, c.ID) + "/"
}
func engineRememberFact(tx gwstore.Tx, c railcontract.Command, f railcontract.Fact, state, reason string, now time.Time) error {
	digest, err := engineHash(f)
	if err != nil {
		return err
	}
	out := engineEvidence{Type: "processor_inquiry", Decision: state, Reason: reason, NormalizedDigest: digest, At: now}
	if f.Command == c && engineValidFact(f) {
		out.Reference = f.Reference
		out.CardBrand = f.CardBrand
		out.RouteID, out.ConnectorID, out.ProviderAccount, out.PaymentMethod = c.RouteID, c.ConnectorID, c.ProviderAccount, c.PaymentMethod
	}
	key := engineEvidencePrefix(c) + "fact_" + digest + "_" + state
	_, found, err := tx.Get(key)
	if found || err != nil {
		return err
	}
	return gwstore.PutJSON(tx, key, out)
}

type engineRecheck struct {
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason_code"`
}

func (s *Service) registerInquiry(m *http.ServeMux) {
	m.HandleFunc("POST /v2/operations/{id}/recheck", controlAuth([]Role{ReviewerRole}, s.recheck))
	m.HandleFunc("GET /v2/operations/{id}/evidence", controlAuth([]Role{ReviewerRole, FinanceRole}, s.evidence))
}
func (s *Service) recheck(w http.ResponseWriter, r *http.Request, p Principal) error {
	id := r.PathValue("id")
	if !engineIdentifier.MatchString(id) {
		return engineFail(404, "operation_not_found")
	}
	key, err := engineIdempotency(r)
	if err != nil {
		return err
	}
	var in engineRecheck
	if err := engineDecode(r, &in); err != nil {
		return err
	}
	if in.ExpectedVersion < 1 || in.Reason != "partner_evidence_available" {
		return engineFail(400, "invalid_recheck")
	}
	digest, err := engineHash(struct {
		ID, Actor string
		Terms     engineRecheck
	}{id, p.Subject, in})
	if err != nil {
		return err
	}
	binding := engineKey("idempotency", p.Merchant, "inquiry", key)
	var op engineOperation
	var replay bool
	now := s.Now().UTC()
	err = s.Store.Update(r.Context(), func(tx gwstore.Tx) error {
		if err := checkEngineEnrollmentTerms(tx, p); err != nil {
			return err
		}
		var err error
		op, replay, err = engineReplay(tx, p, binding, digest)
		if err != nil || replay {
			return err
		}
		op, err = engineGet(tx, p.Merchant, id)
		if err != nil {
			return err
		}
		if !engineVisible(p, op) {
			return engineFail(404, "operation_not_found")
		}
		if op.Status != "review" || op.Ticket != "" || op.Version != in.ExpectedVersion {
			return engineFail(409, "review_state_changed")
		}
		op.InquiryOnly = true
		op.Reason = "reviewer_requested_original_inquiry"
		if err := engineEnqueue(tx, &op, now); err != nil {
			return err
		}
		if err := enginePublish(tx, &op, now); err != nil {
			return err
		}
		if err := gwstore.PutJSON(tx, binding, engineBinding{id, digest}); err != nil {
			return err
		}
		return gwstore.PutJSON(tx, engineEvidencePrefix(op.Command)+"review_"+digest, engineEvidence{Type: "reviewer_inquiry", Decision: "query_original", Reason: in.Reason, Actor: p.Subject, At: now})
	})
	if err != nil {
		return err
	}
	code := 202
	if replay {
		code = 200
	}
	view := enginePresent(op)
	view.CheckoutURL = "" // Buyer presentation credentials are outside the reviewer role.
	engineSend(w, code, view)
	return nil
}
func (s *Service) evidence(w http.ResponseWriter, r *http.Request, p Principal) error {
	id := r.PathValue("id")
	if !engineIdentifier.MatchString(id) {
		return engineFail(404, "operation_not_found")
	}
	after := r.URL.Query().Get("after")
	if after != "" && !engineIdentifier.MatchString(after) {
		return engineFail(400, "invalid_cursor")
	}
	entries := []engineEvidence{}
	next := ""
	err := s.Store.View(r.Context(), func(tx gwstore.Tx) error {
		entries = entries[:0]
		next = ""
		op, err := engineGet(tx, p.Merchant, id)
		if err != nil {
			return err
		}
		if !engineVisible(p, op) {
			return engineFail(404, "operation_not_found")
		}
		if err := checkEngineEnrollmentTerms(tx, p); err != nil {
			return err
		}
		prefix := engineEvidencePrefix(op.Command)
		rows, err := tx.Scan(prefix, prefix+after, 101)
		if err != nil {
			return err
		}
		if len(rows) > 100 {
			next = strings.TrimPrefix(rows[99].Key, prefix)
			rows = rows[:100]
		}
		for _, row := range rows {
			var evidence engineEvidence
			if err := json.Unmarshal(row.Value, &evidence); err != nil {
				return err
			}
			entries = append(entries, evidence)
		}
		return nil
	})
	if err != nil {
		return err
	}
	engineSend(w, 200, struct {
		Evidence []engineEvidence `json:"evidence"`
		Cursor   string           `json:"next_cursor,omitempty"`
	}{entries, next})
	return nil
}

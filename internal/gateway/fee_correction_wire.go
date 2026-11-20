package gateway

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"

	"dakimpay/internal/gwstore"
)

// FeeCorrectionTerms is the v1 signing schema. Field order is part of the signed
// representation. The two source digests identify distinct processor manifests.
type FeeCorrectionTerms struct {
	FinancialRoute
	SourceSubject    string `json:"source_subject"`
	Merchant         string `json:"merchant_id"`
	Account          string `json:"processor_account"`
	Currency         string `json:"currency"`
	Beneficiary      string `json:"beneficiary"`
	ConnectorID      string `json:"connector_id"`
	Operation        string `json:"operation_id"`
	Order            string `json:"order_id"`
	Allocation       string `json:"allocation_id"`
	Capture          string `json:"capture_reference"`
	OriginalPayout   string `json:"original_payout_id"`
	OriginalProposal string `json:"original_proposal_id"`
	OriginalSource   string `json:"original_source_digest"`
	OriginalLine     string `json:"original_line_id"`
	OriginalFee      string `json:"original_fee_reference"`
	OriginalAmount   int64  `json:"original_fee_minor"`
	ID               string `json:"correction_id"`
	Payout           string `json:"corrected_payout_id"`
	Trace            string `json:"bank_trace"`
	Source           string `json:"source_digest"`
	Delta            int64  `json:"delta_minor"`
}

type SignedFeeCorrection struct {
	KeyID     string             `json:"key_id"`
	Terms     FeeCorrectionTerms `json:"terms"`
	Signature string             `json:"signature"`
}

// SigningBytes returns compact JSON for an explicitly versioned, domain-separated
// message. A gateway accepts only typed scalar terms, exact field names and integer
// minor units. Whitespace or key ordering in the outer transport cannot change it.
func (in SignedFeeCorrection) SigningBytes() []byte {
	msg, err := json.Marshal(struct {
		KeyID string             `json:"key_id"`
		Terms FeeCorrectionTerms `json:"terms"`
	}{in.KeyID, in.Terms})
	if err != nil {
		panic(err) // The closed signing schema has only strings and int64 values.
	}
	return append([]byte("dakimpay/settlement-fee-correction/1\n"), msg...)
}

func (in SignedFeeCorrection) verify(p Principal) error {
	t := in.Terms
	if p.Role != StatementRole || p.StatementRoute == "" || p.StatementProviderAccount == "" ||
		t.RouteID != p.StatementRoute || t.ProviderAccount != p.StatementProviderAccount ||
		t.Merchant != p.Merchant || t.Account != p.Account || t.Currency != p.Currency ||
		t.Beneficiary != p.Beneficiary || t.SourceSubject != p.Subject {
		return financialError{403, "fee correction differs from the authenticated statement grant"}
	}
	key, err := hex.DecodeString(p.FeeCorrectionKeys[in.KeyID])
	if err != nil || len(key) != ed25519.PublicKeySize || !engineIdentifier.MatchString(in.KeyID) {
		return financialError{403, "fee correction signing key is not enrolled for this source"}
	}
	sig, err := hex.DecodeString(in.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize || hex.EncodeToString(sig) != in.Signature ||
		!ed25519.Verify(key, in.SigningBytes(), sig) {
		return financialError{403, "fee correction signature is invalid"}
	}
	if !financialCurrency.MatchString(t.Currency) {
		return financialInvalid("invalid fee correction namespace")
	}
	for _, id := range []string{t.RouteID, t.ProviderAccount, t.SourceSubject, t.Merchant, t.Account, t.Beneficiary, t.ConnectorID, t.Operation,
		t.Order, t.Allocation, t.Capture, t.OriginalPayout, t.OriginalLine, t.OriginalFee, t.ID, t.Payout, t.Trace} {
		if !engineIdentifier.MatchString(id) {
			return financialInvalid("missing or invalid fee correction identity")
		}
	}
	if !financialDigest.MatchString(t.OriginalProposal) || !financialDigest.MatchString(t.OriginalSource) ||
		!financialDigest.MatchString(t.Source) || t.OriginalSource == t.Source || t.OriginalPayout == t.Payout {
		return financialInvalid("a distinct corrected settlement and original close evidence are required")
	}
	revised, err := addFinancialAmount(t.OriginalAmount, t.Delta)
	if err != nil || t.OriginalAmount >= 0 || t.OriginalAmount == math.MinInt64 || t.Delta == 0 ||
		t.Delta == math.MinInt64 || revised > 0 || revised == math.MinInt64 {
		return financialInvalid("correction must revise a booked fee in signed minor units")
	}
	return nil
}

func (s *Service) financeFeeCorrection(w http.ResponseWriter, r *http.Request, p Principal) error {
	var in SignedFeeCorrection
	if err := financeJSON(r, &in, "terms", "key_id", "signature"); err != nil {
		return err
	}
	var out payoutRecord
	var created bool
	now := s.Now()
	err := s.Store.Update(r.Context(), func(tx gwstore.Tx) error {
		var err error
		out, created, err = stageFeeCorrection(tx, p, in, now)
		return err
	})
	if err != nil {
		return err
	}
	if created {
		return financeResponse(w, http.StatusCreated, out)
	}
	return financeResponse(w, http.StatusOK, out)
}

func (s *Service) financeConfirmedFeeCorrection(w http.ResponseWriter, r *http.Request, p Principal) error {
	route := payoutRoute(r)
	id := r.PathValue("id")
	if !engineIdentifier.MatchString(route.RouteID) || !engineIdentifier.MatchString(route.ProviderAccount) || !engineIdentifier.MatchString(id) {
		return financialInvalid("invalid corrected settlement identity")
	}
	var out FeeCorrectionExport
	err := s.Store.View(r.Context(), func(tx gwstore.Tx) error {
		var err error
		out, err = confirmedFeeCorrection(tx, p, route, id)
		return err
	})
	if err != nil {
		return err
	}
	return financeResponse(w, http.StatusOK, out)
}

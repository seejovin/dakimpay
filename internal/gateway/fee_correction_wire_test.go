package gateway

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFeeCorrectionSignedWire(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{
		Role: StatementRole, Subject: "processor", Merchant: "shop", Account: "agg", Currency: "USD",
		Beneficiary: "bank-alias", StatementRoute: "east", StatementProviderAccount: "native",
		FeeCorrectionKeys: map[string]string{"issuer": hex.EncodeToString(pub)},
	}
	digestA, digestB, digestC := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	source := SignedFeeCorrection{
		KeyID: "issuer",
		Terms: FeeCorrectionTerms{
			FinancialRoute: FinancialRoute{"east", "native"}, SourceSubject: "processor", Merchant: "shop",
			Account: "agg", Currency: "USD", Beneficiary: "bank-alias", ConnectorID: "east-connector",
			Operation: "charge-01", Order: "sale-01", Allocation: "tender-01", Capture: "cap-01",
			OriginalPayout: "daily-1", OriginalProposal: digestA, OriginalSource: digestB,
			OriginalLine: "fee-line-1", OriginalFee: "fee-1", OriginalAmount: -10,
			ID: "corr-01", Payout: "daily-2", Trace: "bank-2", Source: digestC, Delta: 4,
		},
	}

	// This independent byte vector pins the version, field names, ordering, and
	// signed amounts an issuer must implement in another language.
	wire := "dakimpay/settlement-fee-correction/1\n" +
		`{"key_id":"issuer","terms":{"route_id":"east","provider_account":"native","source_subject":"processor",` +
		`"merchant_id":"shop","processor_account":"agg","currency":"USD","beneficiary":"bank-alias",` +
		`"connector_id":"east-connector","operation_id":"charge-01","order_id":"sale-01","allocation_id":"tender-01",` +
		`"capture_reference":"cap-01","original_payout_id":"daily-1","original_proposal_id":"` + digestA +
		`","original_source_digest":"` + digestB + `","original_line_id":"fee-line-1","original_fee_reference":"fee-1",` +
		`"original_fee_minor":-10,"correction_id":"corr-01","corrected_payout_id":"daily-2","bank_trace":"bank-2",` +
		`"source_digest":"` + digestC + `","delta_minor":4}}`
	if string(source.SigningBytes()) != wire {
		t.Fatal("issuer signing contract changed")
	}
	source.Signature = hex.EncodeToString(ed25519.Sign(priv, []byte(wire)))
	if err := source.verify(p); err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, v SignedFeeCorrection, principal Principal, status int) {
		t.Helper()
		err := v.verify(principal)
		var e financialError
		if !errors.As(err, &e) || e.status != status {
			t.Fatalf("verify returned %v, want HTTP %d", err, status)
		}
	}
	for name, alter := range map[string]func(*SignedFeeCorrection){
		"changed fee":     func(v *SignedFeeCorrection) { v.Terms.OriginalFee = "fee-2" },
		"changed capture": func(v *SignedFeeCorrection) { v.Terms.Capture = "cap-02" },
		"changed delta":   func(v *SignedFeeCorrection) { v.Terms.Delta++ },
		"unknown key":     func(v *SignedFeeCorrection) { v.KeyID = "other" },
		"empty signature": func(v *SignedFeeCorrection) { v.Signature = "" },
	} {
		t.Run(name, func(t *testing.T) {
			v := source
			alter(&v)
			check(t, v, p, http.StatusForbidden)
		})
	}
	for name, alter := range map[string]func(*Principal){
		"bank role":                 func(p *Principal) { p.Role = BankRole },
		"another merchant":          func(p *Principal) { p.Merchant = "other" },
		"another subject":           func(p *Principal) { p.Subject = "other" },
		"another route":             func(p *Principal) { p.StatementRoute = "west" },
		"another processor account": func(p *Principal) { p.StatementProviderAccount = "foreign" },
		"unenrolled source":         func(p *Principal) { p.FeeCorrectionKeys = nil },
	} {
		t.Run(name, func(t *testing.T) {
			other := p
			alter(&other)
			check(t, source, other, http.StatusForbidden)
		})
	}
	for name, alter := range map[string]func(*FeeCorrectionTerms){
		"missing original fee amount": func(v *FeeCorrectionTerms) { v.OriginalAmount = 0 },
		"missing delta":               func(v *FeeCorrectionTerms) { v.Delta = 0 },
		"missing original line":       func(v *FeeCorrectionTerms) { v.OriginalLine = "" },
		"missing settlement":          func(v *FeeCorrectionTerms) { v.Payout = "" },
		"reused settlement":           func(v *FeeCorrectionTerms) { v.Payout = v.OriginalPayout },
		"reused source digest":        func(v *FeeCorrectionTerms) { v.Source = v.OriginalSource },
		"overcorrected fee":           func(v *FeeCorrectionTerms) { v.Delta = 11 },
		"underflow":                   func(v *FeeCorrectionTerms) { v.Delta = math.MinInt64 + 3 },
	} {
		t.Run(name, func(t *testing.T) {
			v := source
			alter(&v.Terms)
			v.Signature = hex.EncodeToString(ed25519.Sign(priv, v.SigningBytes()))
			check(t, v, p, http.StatusUnprocessableEntity)
		})
	}
	t.Run("negative delta and full rebate", func(t *testing.T) {
		for _, delta := range []int64{-4, 10} {
			v := source
			v.Terms.Delta = delta
			v.Signature = hex.EncodeToString(ed25519.Sign(priv, v.SigningBytes()))
			if err := v.verify(p); err != nil {
				t.Fatalf("delta %d: %v", delta, err)
			}
		}
	})

	body, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(nil, nil).Handler()
	for _, tc := range []struct{ name, old, new string }{
		{"repeated nested field", `"delta_minor":4`, `"delta_minor":4,"delta_minor":4`},
		{"case alias", `"original_line_id"`, `"ORIGINAL_LINE_ID"`},
		{"exponent minor units", `"delta_minor":4`, `"delta_minor":4e0`},
		{"self supplied public key", `"key_id":`, `"public_key":"` + hex.EncodeToString(pub) + `","key_id":`},
		{"null signature", `"signature":"` + source.Signature + `"`, `"signature":null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Count(string(body), tc.old) != 1 {
				t.Fatal("malformed test substitution")
			}
			raw := strings.Replace(string(body), tc.old, tc.new, 1)
			req := httptest.NewRequest(http.MethodPost, "/v2/settlement-fee-corrections", strings.NewReader(raw))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req.WithContext(WithPrincipal(req.Context(), p)))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for malformed correction, got %d", w.Code)
			}
		})
	}
}

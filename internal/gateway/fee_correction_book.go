package gateway

import (
	"time"

	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
)

const correctionExportSchema = "dakimpay.confirmed_fee_correction.v1"

// Keep the accepted envelope and the public verification key used at ingress.
// Later key rotations must not change the evidence the reviewer approved.
type bookedFeeCorrection struct {
	Envelope  SignedFeeCorrection `json:"envelope"`
	PublicKey string              `json:"public_key"`
}

type FeeCorrectionExport struct {
	Schema            string             `json:"schema"`
	PostingID         string             `json:"posting_id"`
	Terms             FeeCorrectionTerms `json:"terms"`
	RevisedAmount     int64              `json:"revised_fee_minor"`
	PaymentMethod     string             `json:"payment_method"`
	CardBrand         string             `json:"card_brand,omitempty"`
	ProcessorLedger   string             `json:"processor_ledger"`
	OriginalBankEntry string             `json:"original_bank_entry_id"`
	BankEntry         string             `json:"bank_entry_id"`
	BankSource        string             `json:"bank_source_digest"`
	Proposal          string             `json:"proposal_id"`
	PreparedBy        string             `json:"prepared_by"`
	ApprovedBy        string             `json:"approved_by"`
	ApprovedAt        time.Time          `json:"approved_at"`
	Sequence          int64              `json:"journal_sequence"`
}

func requireBinding(tx gwstore.Tx, key, want string) error {
	var binding string
	found, err := gwstore.GetJSON(tx, key, &binding)
	if err != nil {
		return err
	}
	if !found || binding != want {
		return financialConflict("fee correction lacks its original allocation")
	}
	return nil
}

// Check the current independent evidence as well as the immutable approved
// proposal. A subsequent return or a matching collision suspends eligibility.
func matchedCorrectionBank(tx gwstore.Tx, p Principal, x payoutRecord) (bankRecord, error) {
	var b bankRecord
	if x.Merchant != p.Merchant || x.Account != p.Account || x.Currency != p.Currency || x.Status != "matched" ||
		x.Net == 0 || x.BankEntry == "" || x.ImportedBy == "" || x.PreparedBy == "" || x.PreparedAt == nil ||
		x.ApprovedBy == "" || x.ApprovedAt == nil || x.Returned != 0 || x.FundingMatchAmbiguous {
		return b, financialConflict("fee correction requires an eligible bank-confirmed payout")
	}
	b, err := closeEvidence(tx, p, x, x.BankEntry)
	if err != nil {
		return b, err
	}
	if b.Merchant != p.Merchant || b.Account != p.Account || b.ImportedBy == "" || b.Source == x.Source ||
		x.PreparedBy == x.ImportedBy || x.PreparedBy == b.ImportedBy || x.ApprovedBy == x.PreparedBy ||
		x.ApprovedBy == x.ImportedBy || x.ApprovedBy == b.ImportedBy ||
		x.Proposal != closeProposal(p, x, x.BankEntry, b, x.PreparedBy) {
		return b, financialConflict("fee correction lacks an independent approved close")
	}
	prefix := bookPrefix(p.Merchant, p.Account, p.Currency)
	if err := requireBinding(tx, bookKey(prefix, "bank-allocation", x.BankEntry), bankAllocationOwner(prefix, x.FinancialRoute, x.ID)); err != nil {
		return b, err
	}
	close, found, err := readPosting(tx, prefix, x.FinancialRoute, "settlement_close", x.ID)
	if err != nil {
		return b, err
	}
	expected, err := financialLegs(bankUnallocated, fundingInTransit, x.Net)
	if err != nil {
		return b, err
	}
	if !found || close.Legs != expected || close.Parent != b.ID || close.Source != x.Proposal {
		return b, financialConflict("fee correction lacks its original close posting")
	}
	return b, nil
}

type correctionOrigin struct {
	payout payoutRecord
	op     engineOperation
}

// Resolve the immutable instruction and merchant tender allocation, the confirmed
// capture and the specific fee row in the exact, previously approved manifest.
func originalFeeEvidence(tx gwstore.Tx, p Principal, t FeeCorrectionTerms) (correctionOrigin, error) {
	var origin correctionOrigin
	x, err := payoutByID(tx, p, t.FinancialRoute, t.OriginalPayout)
	if err != nil {
		return origin, err
	}
	if x.Correction != nil || x.Proposal != t.OriginalProposal || x.Source != t.OriginalSource || x.Trace == t.Trace {
		return origin, financialConflict("original close or corrected settlement lineage differs")
	}
	originalBank, err := matchedCorrectionBank(tx, p, x)
	if err != nil {
		return origin, err
	}
	if originalBank.Source == t.Source {
		return origin, financialConflict("corrected processor manifest reuses original bank evidence")
	}
	fee := StatementLine{LineID: t.OriginalLine, Kind: "fee", Reference: t.OriginalFee, Parent: t.Capture, Amount: t.OriginalAmount}
	var captures, matches int
	var captureLine StatementLine
	for _, l := range x.Lines {
		if l.Kind == "capture" && l.Reference == t.Capture {
			captures++
			captureLine = l
		}
		if l == fee {
			matches++
		}
	}
	if matches != 1 || captures != 1 || captureLine.Parent != "" {
		return origin, financialConflict("original approved manifest must contain the exact fee and its capture")
	}
	prefix := bookPrefix(p.Merchant, p.Account, p.Currency)
	source := t.sourcePrefix(prefix)
	if err = requireBinding(tx, bookKey(source, "line", captureLine.LineID), x.ID); err != nil {
		return origin, err
	}
	if err = requireBinding(tx, bookKey(source, "allocation", "capture", t.Capture), x.ID); err != nil {
		return origin, err
	}
	if err = requireBinding(tx, bookKey(source, "line", t.OriginalLine), x.ID); err != nil {
		return origin, err
	}
	if err = requireBinding(tx, bookKey(source, "allocation", "fee", t.OriginalFee), x.ID); err != nil {
		return origin, err
	}
	booked, found, err := readPosting(tx, prefix, t.FinancialRoute, "fee", t.OriginalFee)
	if err != nil {
		return origin, err
	}
	legs, err := financialLegs(processorClearing, merchantProceeds, t.OriginalAmount)
	if err != nil {
		return origin, err
	}
	if !found || booked.Source != x.Source || booked.Parent != t.Capture || booked.Legs != legs || booked.ConnectorID != t.ConnectorID ||
		booked.Merchant != t.Merchant || booked.Account != t.Account || booked.Currency != t.Currency {
		return origin, financialConflict("original fee posting and settlement disagree")
	}
	op, err := capturedFeeOperation(tx, p, t, captureLine.Amount, booked)
	if err != nil {
		return origin, err
	}
	return correctionOrigin{payout: x, op: op}, nil
}

func capturedFeeOperation(tx gwstore.Tx, p Principal, t FeeCorrectionTerms, captureAmount int64, booked journalEntry) (engineOperation, error) {
	op, err := engineGet(tx, p.Merchant, t.Operation)
	if err != nil {
		return engineOperation{}, err
	}
	c := op.Command
	if c.ID != t.Operation || c.Amount <= 0 || c.Amount != captureAmount || c.Kind != "charge" || c.Parent != "" || op.Status != "captured" || op.Reference != t.Capture ||
		op.Allocation != t.Allocation || c.Order != t.Order || c.Merchant != t.Merchant || c.Account != t.Account || c.Currency != t.Currency ||
		c.RouteID != t.RouteID || c.ProviderAccount != t.ProviderAccount || c.ConnectorID != t.ConnectorID ||
		brandEvidenceIssue(railcontract.Fact{Command: c, State: "captured", CardBrand: op.CardBrand}) != "" {
		return engineOperation{}, financialConflict("correction differs from the original captured payment instruction")
	}
	if err = requireBinding(tx, engineKey("allocation", p.Merchant, t.Order, t.Allocation), t.Operation); err != nil {
		return engineOperation{}, err
	}
	var enrolled engineRouteEnrollment
	found, err := gwstore.GetJSON(tx, engineRouteKey(p, t.RouteID), &enrolled)
	if err != nil {
		return engineOperation{}, err
	}
	if !found || !engineLedgerID.MatchString(enrolled.Ledger) || enrolled.Ledger != op.Ledger || enrolled.ProviderAccount != t.ProviderAccount || enrolled.ConnectorID != t.ConnectorID {
		return engineOperation{}, financialConflict("captured acquirer differs from enrolled route")
	}
	prefix := bookPrefix(p.Merchant, p.Account, p.Currency)
	capture, found, err := readPosting(tx, prefix, t.FinancialRoute, "capture", t.Capture)
	if err != nil {
		return engineOperation{}, err
	}
	captureLegs, err := financialLegs(processorClearing, merchantProceeds, c.Amount)
	if err != nil {
		return engineOperation{}, err
	}
	if !found || capture.Source != "" || capture.Parent != "" || capture.Operation != c.ID || capture.Order != c.Order || capture.ConnectorID != c.ConnectorID ||
		capture.Merchant != t.Merchant || capture.Account != t.Account || capture.Currency != t.Currency || capture.Legs != captureLegs ||
		capture.PaymentMethod != c.PaymentMethod || capture.CardBrand != op.CardBrand || booked.PaymentMethod != c.PaymentMethod || booked.CardBrand != op.CardBrand {
		return engineOperation{}, financialConflict("original capture posting and fee disagree")
	}
	var guard releaseGuard
	if _, err := gwstore.GetJSON(tx, bookKey(t.sourcePrefix(prefix), "release-guard", t.Capture), &guard); err != nil {
		return engineOperation{}, err
	}
	if guard.OpenCases != 0 {
		return engineOperation{}, financialConflict("original capture has unresolved financial exceptions")
	}
	return op, nil
}

func stageFeeCorrection(tx gwstore.Tx, p Principal, in SignedFeeCorrection, now time.Time) (payoutRecord, bool, error) {
	var x payoutRecord
	if err := in.verify(p); err != nil {
		return x, false, err
	}
	if err := checkFinanceAuthority(tx, p, false); err != nil {
		return x, false, err
	}
	t := in.Terms
	prefix := bookPrefix(p.Merchant, p.Account, p.Currency)
	source := t.sourcePrefix(prefix)
	key := bookKey(source, "payout", t.Payout)
	found, err := gwstore.GetJSON(tx, key, &x)
	if err != nil {
		return x, false, err
	}
	if found {
		if x.Correction == nil || x.Correction.Envelope.Terms != t {
			return x, false, financialConflict("corrected payout ID already has a different source manifest")
		}
		x, err = payoutByID(tx, p, t.FinancialRoute, t.Payout)
		return x, false, err
	}
	if _, err = originalFeeEvidence(tx, p, t); err != nil {
		return x, false, err
	}
	// Historical fee movements have typed native allocation keys. A movement
	// already consumed through either version cannot also become a correction.
	for _, kind := range []string{"fee", "fee_rebate"} {
		if _, exists, err := tx.Get(bookKey(source, "allocation", kind, t.ID)); err != nil {
			return x, false, err
		} else if exists {
			return x, false, financialConflict("correction reference already booked through a statement")
		}
	}
	for _, ownership := range []string{
		bookKey(source, "fee-correction-target", t.OriginalFee),
		bookKey(source, "allocation", "fee_correction", t.ID),
		bookKey(source, "line", t.ID),
	} {
		if err := allocateRecord(tx, ownership, t.Payout); err != nil {
			return x, false, err
		}
	}
	if err := addMatchCandidate(tx, matchCandidateKey(prefix, "payout", t.Beneficiary, t.Trace, t.Delta)); err != nil {
		return x, false, err
	}
	x = payoutRecord{
		SettlementStatement: correctionStatement(t),
		Correction:          &bookedFeeCorrection{Envelope: in, PublicKey: p.FeeCorrectionKeys[in.KeyID]},
		Merchant:            t.Merchant, Account: t.Account, ImportedBy: p.Subject,
		ImportedAt: now.UTC(), Status: "awaiting_bank",
	}
	return x, true, gwstore.PutJSON(tx, key, x)
}

func correctionStatement(t FeeCorrectionTerms) SettlementStatement {
	return SettlementStatement{
		FinancialRoute: t.FinancialRoute, ID: t.Payout, Trace: t.Trace,
		Beneficiary: t.Beneficiary, Currency: t.Currency, Net: t.Delta, Source: t.Source,
		Lines: []StatementLine{{LineID: t.ID, Kind: "fee_correction", Reference: t.ID, Parent: t.OriginalFee, Amount: t.Delta}},
	}
}

// Used at preparation, approval and current export lookup. A bank return can
// arrive before any payout allocates the original bank entry.
func correctionEvidence(tx gwstore.Tx, p Principal, x payoutRecord, b bankRecord) (correctionOrigin, error) {
	in := x.Correction.Envelope
	t := in.Terms
	// Check the archived signed terms with the key accepted at staging, even
	// if the credential has since rotated. Reviewers do not supply a key.
	historicalSource := Principal{
		Role: StatementRole, Subject: x.ImportedBy, Merchant: x.Merchant, Account: x.Account,
		Currency: x.Currency, Beneficiary: x.Beneficiary, StatementRoute: x.RouteID,
		StatementProviderAccount: x.ProviderAccount, FeeCorrectionKeys: map[string]string{in.KeyID: x.Correction.PublicKey},
	}
	if err := in.verify(historicalSource); err != nil {
		return correctionOrigin{}, financialConflict("stored fee correction lacks its original valid signature")
	}
	if financeHash(x.SettlementStatement) != financeHash(correctionStatement(t)) {
		return correctionOrigin{}, financialConflict("corrected settlement differs from its signed terms")
	}
	origin, err := originalFeeEvidence(tx, p, t)
	if err != nil {
		return origin, err
	}
	if b.ID == "" || b.ID == origin.payout.BankEntry || b.Source == x.Source || b.Source == t.OriginalSource || b.ImportedBy == x.ImportedBy ||
		b.ImportedBy == "" || b.Merchant != p.Merchant || b.Account != p.Account {
		return origin, financialConflict("correction requires separate independent posted bank evidence")
	}
	prefix := bookPrefix(p.Merchant, p.Account, p.Currency)
	var sums returnTotals
	if _, err := gwstore.GetJSON(tx, bookKey(prefix, "return-totals", b.ID), &sums); err != nil {
		return origin, err
	}
	if sums.Returned != 0 {
		return origin, financialConflict("correction bank movement has been returned")
	}
	return origin, nil
}

func approveFeeCorrection(tx gwstore.Tx, p Principal, x payoutRecord, b bankRecord, now time.Time) error {
	origin, err := correctionEvidence(tx, p, x, b)
	if err != nil {
		return err
	}
	if p.Role != ReviewerRole || x.Proposal != closeProposal(p, x, b.ID, b, x.PreparedBy) {
		return financialConflict("correction approval differs from the prepared evidence")
	}
	prefix := bookPrefix(p.Merchant, p.Account, p.Currency)
	if err := requireBinding(tx, bookKey(prefix, "bank-allocation", b.ID), bankAllocationOwner(prefix, x.FinancialRoute, x.ID)); err != nil {
		return err
	}
	t := x.Correction.Envelope.Terms
	op := origin.op
	legs, err := financialLegs(processorClearing, merchantProceeds, t.Delta)
	if err != nil {
		return err
	}
	if err := appendPosting(tx, journalEntry{FinancialRoute: x.FinancialRoute, PaymentMethod: op.Command.PaymentMethod, ConnectorID: t.ConnectorID, CardBrand: op.CardBrand, Merchant: t.Merchant, Account: t.Account, Currency: t.Currency,
		Kind: "fee_correction", Reference: t.ID, Parent: t.OriginalFee, Operation: t.Operation, Order: t.Order, Source: t.Source, BookedAt: now, Legs: legs}); err != nil {
		return err
	}
	legs, err = financialLegs(fundingInTransit, processorClearing, t.Delta)
	if err != nil {
		return err
	}
	if err := appendPosting(tx, journalEntry{FinancialRoute: x.FinancialRoute, Merchant: t.Merchant, Account: t.Account, Currency: t.Currency,
		Kind: "remittance", Reference: x.ID, Source: t.Source, BookedAt: now, Legs: legs}); err != nil {
		return err
	}
	out, err := feeExportFromJournal(tx, x, b, origin, p.Subject, now)
	if err != nil {
		return err
	}
	key := bookKey(x.sourcePrefix(prefix), "confirmed-fee-correction", x.ID)
	if _, exists, err := tx.Get(key); err != nil {
		return err
	} else if exists {
		return financialConflict("correction already has an export")
	}
	return gwstore.PutJSON(tx, key, out)
}

// Build the public confirmation from the posted movement and its approval.
func feeExportFromJournal(tx gwstore.Tx, x payoutRecord, b bankRecord, origin correctionOrigin, approver string, approvedAt time.Time) (FeeCorrectionExport, error) {
	t := x.Correction.Envelope.Terms
	prefix := bookPrefix(t.Merchant, t.Account, t.Currency)
	posted, found, err := readPosting(tx, prefix, x.FinancialRoute, "fee_correction", t.ID)
	if err != nil {
		return FeeCorrectionExport{}, err
	}
	legs, err := financialLegs(processorClearing, merchantProceeds, t.Delta)
	if err != nil {
		return FeeCorrectionExport{}, err
	}
	if !found || posted.Legs != legs || posted.Source != t.Source || posted.Parent != t.OriginalFee || posted.Operation != t.Operation || posted.Order != t.Order ||
		posted.Merchant != t.Merchant || posted.Account != t.Account || posted.Currency != t.Currency || posted.ConnectorID != t.ConnectorID ||
		posted.PaymentMethod != origin.op.Command.PaymentMethod || posted.CardBrand != origin.op.CardBrand || posted.Sequence < 1 {
		return FeeCorrectionExport{}, financialConflict("confirmed correction posting missing or inconsistent")
	}
	revised, err := addFinancialAmount(t.OriginalAmount, t.Delta)
	if err != nil {
		return FeeCorrectionExport{}, err
	}
	return FeeCorrectionExport{
		Schema:    correctionExportSchema,
		PostingID: financeHash([]string{correctionExportSchema, t.Merchant, t.Account, t.Currency, t.RouteID, t.ProviderAccount, t.ID}),
		Terms:     t, RevisedAmount: revised, PaymentMethod: posted.PaymentMethod, CardBrand: posted.CardBrand,
		ProcessorLedger: origin.op.Ledger, OriginalBankEntry: origin.payout.BankEntry,
		BankEntry: b.ID, BankSource: b.Source, Proposal: x.Proposal, PreparedBy: x.PreparedBy, ApprovedBy: approver,
		ApprovedAt: approvedAt.UTC(), Sequence: posted.Sequence,
	}, nil
}

func confirmedFeeCorrection(tx gwstore.Tx, p Principal, route FinancialRoute, id string) (FeeCorrectionExport, error) {
	var out FeeCorrectionExport
	x, err := payoutByID(tx, p, route, id)
	if err != nil {
		return out, err
	}
	if x.Correction == nil {
		return out, financialError{404, "fee correction not found"}
	}
	b, err := matchedCorrectionBank(tx, p, x)
	if err != nil {
		return out, err
	}
	origin, err := correctionEvidence(tx, p, x, b)
	if err != nil {
		return out, err
	}
	key := bookKey(route.sourcePrefix(bookPrefix(p.Merchant, p.Account, p.Currency)), "confirmed-fee-correction", id)
	found, err := gwstore.GetJSON(tx, key, &out)
	if err != nil {
		return out, err
	}
	want, err := feeExportFromJournal(tx, x, b, origin, x.ApprovedBy, *x.ApprovedAt)
	if err != nil {
		return FeeCorrectionExport{}, err
	}
	if !found || out != want {
		return FeeCorrectionExport{}, financialConflict("confirmed correction export missing or inconsistent")
	}
	return out, nil
}

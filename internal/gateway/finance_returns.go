package gateway

import (
	"strings"
	"time"

	"dakimpay/internal/gwstore"
)

// A subsequent return never edits the already-posted deposit or its approval.
// Track the two independent observations: a bank returning money and a partner
// recording that money back in the merchant's processor position.
type returnTotals struct {
	Returned   int64 `json:"returned_minor"`
	Recredited int64 `json:"recredited_minor"`
}

func postBankReturn(tx gwstore.Tx, prefix string, p Principal, b BankEntry, now time.Time) error {
	var original bankRecord
	exists, err := gwstore.GetJSON(tx, bookKey(prefix, "bank", b.ReturnOf), &original)
	if err != nil {
		return err
	}
	if !exists || original.ReturnOf != "" || original.Beneficiary != b.Beneficiary || original.Currency != b.Currency ||
		(original.Amount > 0) == (b.Amount > 0) {
		return financialConflict("returned entry must identify an original opposite-direction posted movement")
	}
	// Do not discard a second real bank debit merely because its cumulative
	// amount exceeds the original deposit. The signed exposure remains visible.
	key := bookKey(prefix, "return-totals", b.ReturnOf)
	var totals returnTotals
	if _, err = gwstore.GetJSON(tx, key, &totals); err != nil {
		return err
	}
	totals.Returned, err = addFinancialAmount(totals.Returned, -b.Amount)
	if err != nil {
		return err
	}
	if err = gwstore.PutJSON(tx, key, totals); err != nil {
		return err
	}
	legs, err := financialLegs(bankReturnSuspense, bankUnallocated, -b.Amount)
	if err != nil {
		return err
	}
	return appendPosting(tx, journalEntry{Merchant: p.Merchant, Account: p.Account, Currency: p.Currency, Kind: "bank_return",
		Reference: b.ID, Parent: b.ReturnOf, Source: b.Source, BookedAt: now, Legs: legs})
}

func postReturnRecredit(tx gwstore.Tx, prefix string, p Principal, x SettlementStatement, l StatementLine, now time.Time) error {
	var returned bankRecord
	found, err := gwstore.GetJSON(tx, bookKey(prefix, "bank", l.Parent), &returned)
	if err != nil {
		return err
	}
	if !found || returned.ReturnOf == "" || returned.Beneficiary != x.Beneficiary || returned.Currency != x.Currency ||
		(returned.Amount > 0) == (l.Amount > 0) || returned.ImportedBy == p.Subject {
		return financialConflict("partner recredit requires an independently posted opposite-direction bank return")
	}
	var originalPayoutID string
	found, err = gwstore.GetJSON(tx, bookKey(prefix, "bank-allocation", returned.ReturnOf), &originalPayoutID)
	if err != nil {
		return err
	}
	if !found {
		return financialConflict("returned bank movement is not linked to an original matched payout")
	}
	var original payoutRecord
	key := bookKey(prefix, "payout", originalPayoutID)
	if routed, ok := strings.CutPrefix(originalPayoutID, "route/"); ok {
		key = routed
	}
	found, err = gwstore.GetJSON(tx, key, &original)
	if err != nil {
		return err
	}
	if !found || original.Status != "matched" || original.BankEntry != returned.ReturnOf || original.FinancialRoute != x.FinancialRoute {
		return financialConflict("original payout must be independently closed before re-funding")
	}
	if ambiguous, err := ambiguousFunding(tx, prefix, original); err != nil {
		return err
	} else if ambiguous {
		return financialConflict("original funding has conflicting source evidence")
	}
	key = bookKey(prefix, "return-recredit-allocation", returned.ID)
	var prior int64
	if _, err = gwstore.GetJSON(tx, key, &prior); err != nil {
		return err
	}
	applied, err := addFinancialAmount(prior, l.Amount)
	if err != nil {
		return err
	}
	if returned.Amount < 0 && applied > -returned.Amount || returned.Amount > 0 && applied < -returned.Amount {
		return financialConflict("partner recredit exceeds the identified bank return")
	}
	if err = gwstore.PutJSON(tx, key, applied); err != nil {
		return err
	}
	key = bookKey(prefix, "return-totals", returned.ReturnOf)
	var totals returnTotals
	if _, err = gwstore.GetJSON(tx, key, &totals); err != nil {
		return err
	}
	totals.Recredited, err = addFinancialAmount(totals.Recredited, l.Amount)
	if err != nil {
		return err
	}
	if err = gwstore.PutJSON(tx, key, totals); err != nil {
		return err
	}
	legs, err := financialLegs(processorClearing, bankReturnSuspense, l.Amount)
	if err != nil {
		return err
	}
	return appendPosting(tx, journalEntry{FinancialRoute: x.FinancialRoute, Merchant: p.Merchant, Account: p.Account, Currency: p.Currency, Kind: l.Kind,
		Reference: l.Reference, Parent: l.Parent, Source: x.Source, BookedAt: now, Legs: legs})
}

func refreshBankReturns(tx gwstore.Tx, prefix string, payout *payoutRecord) error {
	var totals returnTotals
	_, err := gwstore.GetJSON(tx, bookKey(prefix, "return-totals", payout.BankEntry), &totals)
	if err == nil {
		payout.Returned, payout.ProcessorRecredit = totals.Returned, totals.Recredited
	}
	return err
}

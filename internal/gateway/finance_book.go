package gateway

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
)

// One sequencer per merchant program makes a stable, snapshot-paginated journal.
// All tokens derived from user or processor input are hashed as component tuples.
func bookPrefix(merchant, account, currency string) string {
	return "finance/" + financeHash([]string{merchant, account, currency}) + "/"
}

// Independent source evidence must stay with the same beneficiary across process
// restarts and grant rotations. An empty book may take its initial binding only
// in the transaction that accepts its first source record.
func checkBookBeneficiary(tx gwstore.Tx, prefix, alias string, bind bool) error {
	if alias == "" {
		return financialError{403, "beneficiary authority required"}
	}
	var stored string
	found, err := gwstore.GetJSON(tx, prefix+"beneficiary", &stored)
	if err != nil {
		return err
	}
	if found && alias != stored {
		return financialError{403, "beneficiary differs from the program authority"}
	}
	if !found && bind {
		return gwstore.PutJSON(tx, prefix+"beneficiary", alias)
	}
	return nil
}

func checkFinanceAuthority(tx gwstore.Tx, p Principal, bind bool) error {
	if err := checkEngineEnrollmentTerms(tx, p); err != nil {
		return err
	}
	return checkBookBeneficiary(tx, bookPrefix(p.Merchant, p.Account, p.Currency), p.Beneficiary, bind)
}
func bookKey(prefix, class string, components ...string) string {
	return prefix + class + "/" + financeHash(components)
}
func entryKey(prefix string, seq int64) string { return fmt.Sprintf("%sjournal/%019d", prefix, seq) }

type journalBinding struct {
	Terms    string `json:"terms"`
	Sequence int64  `json:"sequence"`
}

func appendPosting(tx gwstore.Tx, e journalEntry) error {
	debit, credit := e.Legs[0], e.Legs[1]
	if !e.FinancialRoute.valid() || e.Merchant == "" || e.Account == "" || !financialCurrency.MatchString(e.Currency) || e.Kind == "" || e.Reference == "" ||
		debit.Account == "" || credit.Account == "" || debit.Account == credit.Account ||
		debit.Debit <= 0 || debit.Credit != 0 || credit.Debit != 0 || credit.Credit != debit.Debit {
		return financialInvalid("journal entry requires a single balanced, scoped movement")
	}
	prefix := bookPrefix(e.Merchant, e.Account, e.Currency)
	key := bookKey(e.sourcePrefix(prefix), "effect", e.Kind, e.Reference)
	when := e.BookedAt.UTC()
	e.BookedAt = time.Time{}
	e.Template = "direct-merchant-control/1"
	terms := financeHash(e)
	var binding journalBinding
	found, err := gwstore.GetJSON(tx, key, &binding)
	if err != nil {
		return err
	}
	if found {
		if binding.Terms != terms {
			return financialConflict("source movement reuses a reference with different terms")
		}
		return nil
	}
	var seq int64
	if _, err = gwstore.GetJSON(tx, prefix+"sequence", &seq); err != nil {
		return err
	}
	if seq == math.MaxInt64 {
		return financialConflict("journal sequence exhausted")
	}
	e.Sequence, e.BookedAt = seq+1, when
	row := entryKey(prefix, e.Sequence)
	if _, exists, err := tx.Get(row); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("journal sequence already recorded")
	}
	if err = gwstore.PutJSON(tx, key, journalBinding{terms, e.Sequence}); err != nil {
		return err
	}
	if err = gwstore.PutJSON(tx, row, e); err != nil {
		return err
	}
	return gwstore.PutJSON(tx, prefix+"sequence", e.Sequence)
}

func readPosting(tx gwstore.Tx, prefix string, route FinancialRoute, kind, reference string) (journalEntry, bool, error) {
	var b journalBinding
	var e journalEntry
	ok, err := gwstore.GetJSON(tx, bookKey(route.sourcePrefix(prefix), "effect", kind, reference), &b)
	if err != nil || !ok {
		return e, false, err
	}
	ok, err = gwstore.GetJSON(tx, entryKey(prefix, b.Sequence), &e)
	if err == nil && (!ok || e.FinancialRoute != route || e.Reference != reference || e.Kind != kind) {
		return e, false, fmt.Errorf("journal identity does not resolve to its original scoped entry")
	}
	return e, ok, err
}

// postConfirmedMovement is called only during verified processor result acceptance. The
// operation identity, economic movement and merchant-facing state share one commit.
func postConfirmedMovement(tx gwstore.Tx, c railcontract.Command, reference, cardBrand string, now time.Time) error {
	if c.Amount <= 0 || c.Merchant == "" || c.Account == "" || c.ID == "" || reference == "" || !financialCurrency.MatchString(c.Currency) {
		return financialInvalid("incomplete confirmed financial movement")
	}
	kind, signed := "capture", c.Amount
	switch c.Kind {
	case "charge":
	case "refund":
		kind, signed = "refund", -c.Amount
	default:
		return financialInvalid("unsupported confirmed financial movement")
	}
	legs, err := financialLegs(processorClearing, merchantProceeds, signed)
	if err != nil {
		return err
	}
	prefix := bookPrefix(c.Merchant, c.Account, c.Currency)
	// A single command in this profile produces a single movement. Surface a
	// changed reference rather than treating it as an ordinary duplicate.
	key := bookKey(prefix, "operation", c.Kind, c.ID)
	route := FinancialRoute{RouteID: c.RouteID, ProviderAccount: c.ProviderAccount}
	// Preserve the historical raw binding for legacy commands. Slash is outside
	// the accepted native reference alphabet, so routed tuples cannot alias it.
	binding := reference
	if route != (FinancialRoute{}) {
		binding = "route/" + financeHash([]string{route.RouteID, route.ProviderAccount, reference})
	}
	var previous string
	found, err := gwstore.GetJSON(tx, key, &previous)
	if err != nil {
		return err
	}
	if found && previous != binding {
		return financialConflict("processor changed the movement reference for this operation")
	}
	if !found {
		if err = gwstore.PutJSON(tx, key, binding); err != nil {
			return err
		}
	}
	return appendPosting(tx, journalEntry{FinancialRoute: route, PaymentMethod: c.PaymentMethod, ConnectorID: c.ConnectorID, CardBrand: cardBrand, Merchant: c.Merchant, Account: c.Account, Currency: c.Currency, Kind: kind,
		Reference: reference, Operation: c.ID, Order: c.Order, Parent: c.Parent, BookedAt: now, Legs: legs})
}

func allocateRecord(tx gwstore.Tx, key, owner string) error {
	var existing string
	ok, err := gwstore.GetJSON(tx, key, &existing)
	if err != nil {
		return err
	}
	if ok && existing != owner {
		return financialConflict("financial source already allocated to a different payout")
	}
	if ok {
		return nil
	}
	return gwstore.PutJSON(tx, key, owner)
}

func importStatement(tx gwstore.Tx, p Principal, x SettlementStatement, now time.Time) (payoutRecord, bool, error) {
	prefix := bookPrefix(p.Merchant, p.Account, p.Currency)
	key := bookKey(x.sourcePrefix(prefix), "payout", x.ID)
	var result payoutRecord
	if !x.FinancialRoute.valid() || x.RouteID != p.StatementRoute || x.ProviderAccount != p.StatementProviderAccount {
		return result, false, financialError{403, "statement source differs from authenticated import grant"}
	}
	if x.RouteID != "" {
		found, err := engineRouteEnrolled(tx, p, x.RouteID, x.ProviderAccount)
		if err != nil {
			return result, false, err
		}
		if !found {
			return result, false, financialConflict("processor account is not enrolled for this merchant route")
		}
	}
	if err := checkFinanceAuthority(tx, p, true); err != nil {
		return result, false, err
	}
	found, err := gwstore.GetJSON(tx, key, &result)
	if err != nil {
		return result, false, err
	}
	if found {
		if result.Correction != nil || financeHash(result.SettlementStatement) != financeHash(x) {
			return result, false, financialConflict("payout ID already has a different source manifest")
		}
		result, err = payoutByID(tx, p, x.FinancialRoute, x.ID)
		return result, false, err
	}
	// Apply all effects, allocations and funding inside one transaction. A
	// missing original leaves no partially consumed statement lines.
	ordered := append([]StatementLine(nil), x.Lines...)
	// A fee can refer to a newly observed second capture. A reversal can refer
	// to a dispute debit in the same manifest, irrespective of file ordering.
	rank := func(kind string) int {
		switch kind {
		case "additional_capture":
			return 0
		case "chargeback_reversal":
			return 2
		}
		return 1
	}
	sort.SliceStable(ordered, func(i, j int) bool { return rank(ordered[i].Kind) < rank(ordered[j].Kind) })
	var exceptions []string
	for _, line := range ordered {
		if line.Kind == "additional_capture" {
			exceptions = append(exceptions, line.Reference)
		}
		if err = postStatementLine(tx, prefix, p, x, line, now); err != nil {
			return result, false, err
		}
	}
	result = payoutRecord{SettlementStatement: x, Merchant: p.Merchant, Account: p.Account, ImportedBy: p.Subject, ImportedAt: now.UTC(), Status: "in_transit", Exceptions: exceptions}
	if x.Net == 0 {
		result.Status = "zero_net"
	} else {
		if e := addMatchCandidate(tx, matchCandidateKey(prefix, "payout", x.Beneficiary, x.Trace, x.Net)); e != nil {
			return result, false, e
		}
		legs, e := financialLegs(fundingInTransit, processorClearing, x.Net)
		if e != nil {
			return result, false, e
		}
		if e = appendPosting(tx, journalEntry{FinancialRoute: x.FinancialRoute, Merchant: p.Merchant, Account: p.Account, Currency: p.Currency, Kind: "remittance",
			Reference: x.ID, Source: x.Source, BookedAt: now, Legs: legs}); e != nil {
			return result, false, e
		}
	}
	if err = refreshExceptions(tx, prefix, &result); err != nil {
		return result, false, err
	}
	return result, true, gwstore.PutJSON(tx, key, result)
}

// A separately identified partner movement can reveal a second collection for
// one original command. Track confirmed partner restitution without changing
// the original intent or issuing another retail release.
type captureException struct {
	Original string `json:"original_capture"`
	Amount   int64  `json:"amount_minor"`
	Refunded int64  `json:"refunded_minor"`
}

func refreshExceptions(tx gwstore.Tx, prefix string, x *payoutRecord) error {
	x.OpenExceptions = nil
	for _, ref := range x.Exceptions {
		var ex captureException
		ok, err := gwstore.GetJSON(tx, bookKey(x.sourcePrefix(prefix), "excess", ref), &ex)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("missing excess collection case")
		}
		if ex.Refunded != ex.Amount {
			x.OpenExceptions = append(x.OpenExceptions, ref)
		}
	}
	return nil
}

func postStatementLine(tx gwstore.Tx, prefix string, p Principal, x SettlementStatement, l StatementLine, now time.Time) error {
	source := x.sourcePrefix(prefix)
	if l.Kind == "fee_correction" {
		return financialInvalid("fee correction requires a separately signed settlement")
	}
	if l.Kind == "fee" || l.Kind == "fee_rebate" {
		if _, reserved, err := tx.Get(bookKey(source, "allocation", "fee_correction", l.Reference)); err != nil {
			return err
		} else if reserved {
			return financialConflict("fee reference is already reserved for a signed correction")
		}
	}
	if l.Kind == "capture" || l.Kind == "refund" {
		entry, found, err := readPosting(tx, prefix, x.FinancialRoute, l.Kind, l.Reference)
		if err != nil {
			return err
		}
		signed := entry.Legs[0].Debit
		if l.Kind == "refund" {
			signed = -signed
		}
		if !found || signed != l.Amount {
			return financialConflict("statement payment line lacks its matching confirmed movement")
		}
	} else if l.Kind == "return_recredit" {
		if err := postReturnRecredit(tx, prefix, p, x, l, now); err != nil {
			return err
		}
	} else {
		// Fees and dispute debits link to the original capture. A dispute
		// reversal links to the external dispute debit. Distinct adverse debits
		// remain bookable even if their total exceeds voluntary refund limits.
		parentKind := "capture"
		if l.Kind == "chargeback_reversal" {
			parentKind = "chargeback"
		}
		parent, found, err := readPosting(tx, prefix, x.FinancialRoute, parentKind, l.Parent)
		if err != nil {
			return err
		}
		if !found {
			return financialConflict("statement adjustment lacks original source evidence")
		}
		legs, err := financialLegs(processorClearing, merchantProceeds, l.Amount)
		if err != nil {
			return err
		}
		kind, operation, order := l.Kind, "", ""
		if l.Kind == "source_refund" {
			kind, order = "refund", parent.Order
			if parent.Parent != "" {
				return financialConflict("use excess-capture restitution for a refund of an excess capture")
			}
			if e := changeReleaseGuard(tx, source, parent.Reference, 0, -l.Amount); e != nil {
				return e
			}
		}
		if l.Kind == "chargeback" {
			if e := openReleaseCase(tx, source, "dispute", l.Reference, releaseAnchor(parent), -l.Amount); e != nil {
				return e
			}
		}
		if l.Kind == "chargeback_reversal" {
			if e := restoreReleaseCase(tx, source, "dispute", l.Parent, l.Amount); e != nil {
				return e
			}
		}
		if l.Kind == "additional_capture" {
			// Independently observed overcollection is recorded in the normal
			// capture reference namespace and never authorizes a retail release.
			kind, operation, order = "capture", parent.Operation, parent.Order
			if parent.Parent != "" {
				return financialConflict("link an excess collection to the original gateway capture")
			}
			if e := openReleaseCase(tx, source, "excess", l.Reference, parent.Reference, l.Amount); e != nil {
				return e
			}
		} else if l.Kind == "refund_excess_capture" {
			kind, order = "refund", parent.Order
			if e := restoreReleaseCase(tx, source, "excess", l.Parent, -l.Amount); e != nil {
				return e
			}
		}
		if err = appendPosting(tx, journalEntry{FinancialRoute: x.FinancialRoute, PaymentMethod: parent.PaymentMethod, ConnectorID: parent.ConnectorID, CardBrand: parent.CardBrand, Merchant: p.Merchant, Account: p.Account, Currency: p.Currency, Kind: kind,
			Reference: l.Reference, Parent: l.Parent, Operation: operation, Order: order, Source: x.Source, BookedAt: now, Legs: legs}); err != nil {
			return err
		}
	}
	if err := allocateRecord(tx, bookKey(source, "line", l.LineID), x.ID); err != nil {
		return err
	}
	kind := l.Kind
	if kind == "additional_capture" {
		kind = "capture"
	} else if kind == "refund_excess_capture" || kind == "source_refund" {
		kind = "refund"
	}
	return allocateRecord(tx, bookKey(source, "allocation", kind, l.Reference), x.ID)
}

func matchCandidateKey(prefix, kind, beneficiary, trace string, amount int64) string {
	return bookKey(prefix, kind+"-candidates", beneficiary, trace, strconv.FormatInt(amount, 10))
}
func addMatchCandidate(tx gwstore.Tx, key string) error {
	var count int64
	if _, err := gwstore.GetJSON(tx, key, &count); err != nil {
		return err
	}
	if count == math.MaxInt64 {
		return financialConflict("matching candidate count exceeds limit")
	}
	return gwstore.PutJSON(tx, key, count+1)
}

func importBank(tx gwstore.Tx, p Principal, b BankEntry, now time.Time) (bankRecord, bool, error) {
	prefix := bookPrefix(p.Merchant, p.Account, p.Currency)
	key := bookKey(prefix, "bank", b.ID)
	var result bankRecord
	if err := checkFinanceAuthority(tx, p, true); err != nil {
		return result, false, err
	}
	found, err := gwstore.GetJSON(tx, key, &result)
	if err != nil {
		return result, false, err
	}
	if found {
		if result.BankEntry != b {
			return result, false, financialConflict("bank entry ID already has different evidence")
		}
		return result, false, nil
	}
	legs, err := financialLegs(bankObserved, bankUnallocated, b.Amount)
	if err != nil {
		return result, false, err
	}
	if err = appendPosting(tx, journalEntry{Merchant: p.Merchant, Account: p.Account, Currency: p.Currency, Kind: "bank_posting",
		Reference: b.ID, Source: b.Source, BookedAt: now, Legs: legs}); err != nil {
		return result, false, err
	}
	if b.ReturnOf == "" {
		if err = addMatchCandidate(tx, matchCandidateKey(prefix, "bank", b.Beneficiary, b.Trace, b.Amount)); err != nil {
			return result, false, err
		}
	}
	if b.ReturnOf != "" {
		if e := postBankReturn(tx, prefix, p, b, now); e != nil {
			return result, false, e
		}
	}
	result = bankRecord{BankEntry: b, Merchant: p.Merchant, Account: p.Account, ImportedBy: p.Subject, ImportedAt: now.UTC()}
	return result, true, gwstore.PutJSON(tx, key, result)
}

func payoutByID(tx gwstore.Tx, p Principal, route FinancialRoute, id string) (payoutRecord, error) {
	var out payoutRecord
	if !route.valid() || !financialID.MatchString(id) {
		return out, financialInvalid("invalid payout identifier")
	}
	prefix := bookPrefix(p.Merchant, p.Account, p.Currency)
	if err := checkFinanceAuthority(tx, p, false); err != nil {
		return out, err
	}
	found, err := gwstore.GetJSON(tx, bookKey(route.sourcePrefix(prefix), "payout", id), &out)
	if err == nil && (!found || out.Beneficiary != p.Beneficiary || out.FinancialRoute != route) {
		err = financialError{404, "payout not found"}
	}
	if err == nil {
		if err = refreshExceptions(tx, prefix, &out); err == nil && out.BankEntry != "" {
			if err = refreshBankReturns(tx, prefix, &out); err == nil {
				out.FundingMatchAmbiguous, err = ambiguousFunding(tx, prefix, out)
			}
		}
	}
	return out, err
}

// Newly received source evidence may introduce a second candidate after
// close. Preserve the original posting and expose the conflict on inquiry.
func ambiguousFunding(tx gwstore.Tx, prefix string, x payoutRecord) (bool, error) {
	for _, kind := range []string{"bank", "payout"} {
		var count int64
		if _, err := gwstore.GetJSON(tx, matchCandidateKey(prefix, kind, x.Beneficiary, x.Trace, x.Net), &count); err != nil {
			return false, err
		}
		if count != 1 {
			return true, nil
		}
	}
	return false, nil
}

func closeEvidence(tx gwstore.Tx, p Principal, x payoutRecord, entry string) (bankRecord, error) {
	var b bankRecord
	if len(x.OpenExceptions) != 0 {
		return b, financialConflict("a separately evidenced additional capture requires restitution review before close")
	}
	if x.Net == 0 {
		if entry != "" {
			return b, financialConflict("a zero-net close cannot allocate a bank movement")
		}
		return b, nil
	}
	if !financialID.MatchString(entry) {
		return b, financialConflict("a nonzero payout requires a matching posted bank entry")
	}
	prefix := bookPrefix(p.Merchant, p.Account, p.Currency)
	found, err := gwstore.GetJSON(tx, bookKey(prefix, "bank", entry), &b)
	if err != nil {
		return b, err
	}
	if !found || b.ReturnOf != "" || x.Currency != b.Currency || x.Beneficiary != b.Beneficiary || x.Trace != b.Trace || x.Net != b.Amount {
		return b, financialConflict("bank entry does not substantiate the payout")
	}
	ambiguous, err := ambiguousFunding(tx, prefix, x)
	if err != nil {
		return b, err
	}
	if ambiguous {
		return b, financialConflict("multiple or missing candidates for this payout and bank entry")
	}
	if b.ImportedBy == x.ImportedBy {
		return b, financialConflict("processor and bank evidence require independent source identities")
	}
	return b, nil
}

func bankAllocationOwner(prefix string, route FinancialRoute, id string) string {
	if route == (FinancialRoute{}) {
		return id
	}
	return "route/" + bookKey(route.sourcePrefix(prefix), "payout", id)
}

func closeProposal(p Principal, x payoutRecord, entry string, b bankRecord, preparer string) string {
	parts := []string{"close/1", p.Merchant, p.Account, p.Currency, x.ID, financeHash(x.SettlementStatement), entry, financeHash(b.BankEntry), preparer}
	if x.Correction != nil {
		parts[0] = "booked-fee-correction-close/1"
		parts = append(parts, financeHash(x.Correction))
	}
	return financeHash(parts)
}

func prepareClose(tx gwstore.Tx, p Principal, route FinancialRoute, id, entry string, now time.Time) (payoutRecord, error) {
	x, err := payoutByID(tx, p, route, id)
	if err != nil {
		return x, err
	}
	if x.Proposal != "" {
		if x.PreparedBy != p.Subject || x.BankEntry != entry {
			return x, financialConflict("a different close is already prepared")
		}
		return x, nil
	}
	b, err := closeEvidence(tx, p, x, entry)
	if err != nil {
		return x, err
	}
	if x.Correction != nil {
		if _, err := correctionEvidence(tx, p, x, b); err != nil {
			return x, err
		}
	}
	proposal := closeProposal(p, x, entry, b, p.Subject)
	if p.Subject == x.ImportedBy || entry != "" && p.Subject == b.ImportedBy {
		return x, financialError{403, "a source importer cannot prepare its close"}
	}
	prefix := bookPrefix(p.Merchant, p.Account, p.Currency)
	if entry != "" {
		owner := bankAllocationOwner(prefix, route, id)
		if err = allocateRecord(tx, bookKey(prefix, "bank-allocation", entry), owner); err != nil {
			return x, err
		}
	}
	now = now.UTC()
	x.Status, x.Proposal, x.BankEntry, x.PreparedBy, x.PreparedAt = "prepared", proposal, entry, p.Subject, &now
	return x, gwstore.PutJSON(tx, bookKey(route.sourcePrefix(prefix), "payout", id), x)
}

func approveClose(tx gwstore.Tx, p Principal, route FinancialRoute, id, proposal string, now time.Time) (payoutRecord, error) {
	x, err := payoutByID(tx, p, route, id)
	if err != nil {
		return x, err
	}
	if proposal == "" || x.Proposal != proposal {
		return x, financialConflict("approval differs from the exact prepared proposal")
	}
	if x.ApprovedBy != "" {
		if x.ApprovedBy != p.Subject {
			return x, financialConflict("a different reviewer already approved this proposal")
		}
		return x, nil
	}
	b, err := closeEvidence(tx, p, x, x.BankEntry)
	if err != nil {
		return x, err
	}
	if p.Subject == x.PreparedBy || p.Subject == x.ImportedBy || x.BankEntry != "" && p.Subject == b.ImportedBy {
		return x, financialError{403, "reviewer must be independent of preparer and sources"}
	}
	if x.Status != "prepared" {
		return x, financialConflict("payout requires a prepared close")
	}
	now = now.UTC()
	if x.Correction != nil {
		if err := approveFeeCorrection(tx, p, x, b, now); err != nil {
			return x, err
		}
	}
	if x.Net != 0 {
		legs, e := financialLegs(bankUnallocated, fundingInTransit, x.Net)
		if e != nil {
			return x, e
		}
		err = appendPosting(tx, journalEntry{FinancialRoute: x.FinancialRoute, Merchant: p.Merchant, Account: p.Account, Currency: p.Currency, Kind: "settlement_close",
			Reference: id, Parent: x.BankEntry, Source: proposal, BookedAt: now, Legs: legs})
		if err != nil {
			return x, err
		}
		x.Status = "matched"
	} else {
		x.Status = "closed_zero_net"
	}
	x.ApprovedBy, x.ApprovedAt = p.Subject, &now
	return x, gwstore.PutJSON(tx, bookKey(route.sourcePrefix(bookPrefix(p.Merchant, p.Account, p.Currency)), "payout", id), x)
}

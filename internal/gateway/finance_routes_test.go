package gateway

import (
	"net/http"
	"strings"
	"testing"

	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
)

func qualifyFinancialRoutes(t *testing.T, store gwstore.Store) {
	ctx := t.Context()
	s := New(store, nil)
	mux := http.NewServeMux()
	s.registerFinance(mux)
	req := financeRequest(t, mux)
	merchant := engineTestPrincipal()
	merchant.Merchant = "finance-routes-" + financeTestID(t)
	principal := func(role Role, subject string) Principal {
		p := merchant
		p.Role, p.Subject = role, subject
		return p
	}
	legacy := principal(StatementRole, "legacy")
	bank := principal(BankRole, "bank")
	maker := principal(FinanceRole, "maker")
	checker := principal(ReviewerRole, "checker")
	type source struct {
		FinancialRoute
		importer Principal
		charge   railcontract.Command
		path     string
	}
	const movement = "000001"
	now := s.Now()
	pay := func(route, method, connector, observed string) source {
		// Both processors happen to issue the same account identifier, payout ID,
		// line ID and movement reference, for two equal amounts and the same merchant.
		const account = "00001"
		c := railcontract.Command{Merchant: merchant.Merchant, Account: merchant.Account, Kind: "charge", ID: "op-" + route, Order: "order-" + route,
			Currency: merchant.Currency, Amount: 1000, RouteID: route, ProviderAccount: account, PaymentMethod: method, ConnectorID: connector}
		if err := store.Update(ctx, func(tx gwstore.Tx) error {
			if err := engineEnroll(tx, merchant, c, financeHash([]string{merchant.Merchant, connector}), ""); err != nil {
				return err
			}
			if err := postConfirmedMovement(tx, c, movement, observed, now); err != nil {
				return err
			}
			refund := c
			refund.Kind, refund.Parent, refund.ID, refund.Amount = "refund", c.ID, "refund-"+route, 100
			return postConfirmedMovement(tx, refund, "RF-1", "", now)
		}); err != nil {
			t.Fatal(err)
		}
		imp := principal(StatementRole, "importer-"+route)
		imp.StatementRoute, imp.StatementProviderAccount = route, account
		return source{FinancialRoute: FinancialRoute{RouteID: route, ProviderAccount: account}, importer: imp, charge: c,
			path: "/v1/routes/" + route + "/accounts/" + account + "/payouts/"}
	}
	a := pay("acquirer", "card", "partner-cards", "visa")
	b := pay("wallet", "wechat_pay", "partner-wallet", "")
	digest := strings.Repeat("a", 64)
	report := func(src source) SettlementStatement {
		return SettlementStatement{FinancialRoute: src.FinancialRoute, ID: "daily", Trace: "trace-" + src.RouteID, Beneficiary: merchant.Beneficiary,
			Currency: merchant.Currency, Source: digest, Net: 900, Lines: []StatementLine{{"1", "capture", movement, "", 1000}, {"2", "refund", "RF-1", "", -100}}}
	}
	xa, xb := report(a), report(b)
	const ingest = "/v1/settlement-statements"
	req(legacy, "POST", ingest, xa, 403, nil)
	req(a.importer, "POST", ingest, xb, 403, nil)
	noRoute := xa
	noRoute.FinancialRoute = FinancialRoute{}
	req(legacy, "POST", ingest, noRoute, 409, nil)
	wrongAccount := xa
	wrongAccount.ProviderAccount = "unassigned"
	req(a.importer, "POST", ingest, wrongAccount, 403, nil)
	unknown := a.importer
	unknown.StatementProviderAccount = "unassigned"
	req(unknown, "POST", ingest, wrongAccount, 409, nil)
	missing := xa
	missing.ProviderAccount = ""
	req(a.importer, "POST", ingest, missing, 422, nil)
	for _, src := range []source{a, b} {
		x := report(src)
		req(src.importer, "POST", ingest, x, 201, nil)
		req(src.importer, "POST", ingest, x, 200, nil)
		entry := BankEntry{ID: src.RouteID + "-booked", Trace: x.Trace, Beneficiary: x.Beneficiary, Currency: x.Currency, Source: digest, Amount: x.Net}
		req(bank, "POST", "/v1/bank-entries", entry, 201, nil)
		var payout payoutRecord
		req(maker, "POST", src.path+x.ID+"/prepare-close", map[string]string{"bank_entry_id": entry.ID}, 200, &payout)
		req(checker, "POST", src.path+x.ID+"/approve-close", map[string]string{"proposal_id": payout.Proposal}, 200, &payout)
		if payout.Status != "matched" || payout.FinancialRoute != src.FinancialRoute {
			t.Fatal("wrong scoped close", payout)
		}
	}
	req(merchant, "GET", "/v1/payouts/daily", nil, 404, nil)

	debit := xa
	debit.ID, debit.Trace, debit.Net = "case", "chargeback-trace", -100
	debit.Lines = []StatementLine{{"case-1", "chargeback", "CB-1", movement, -100}}
	req(a.importer, "POST", ingest, debit, 201, nil)
	checkHeld := func(src source, expected bool) {
		err := store.View(ctx, func(tx gwstore.Tx) error {
			held, err := releaseFinancialHold(tx, src.charge, movement)
			if err == nil && held != expected {
				t.Fatalf("%s held=%t, want %t", src.RouteID, held, expected)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	checkHeld(a, true)
	checkHeld(b, false)
	returned := BankEntry{ID: "returned", Trace: "returned-funds", ReturnOf: a.RouteID + "-booked", Beneficiary: merchant.Beneficiary, Currency: "USD", Source: digest, Amount: -500}
	req(bank, "POST", "/v1/bank-entries", returned, 201, nil)
	recredit := xb
	recredit.ID, recredit.Trace, recredit.Net = "recredit", "new-credit-trace", 500
	recredit.Lines = []StatementLine{{"recredit-line", "return_recredit", "credit", returned.ID, 500}}
	req(b.importer, "POST", ingest, recredit, 409, nil)
	recredit.FinancialRoute = a.FinancialRoute
	req(a.importer, "POST", ingest, recredit, 201, nil)
	var page struct {
		Entries []journalEntry `json:"entries"`
	}
	req(merchant, "GET", "/v1/journal", nil, 200, &page)
	verified := map[string]bool{}
	for i, row := range page.Entries {
		if row.Sequence != int64(i+1) || row.Legs[0].Debit != row.Legs[1].Credit {
			t.Fatal("journal lost continuity or balance", row)
		}
		if row.Kind != "capture" {
			continue
		}
		if row.Reference != movement || verified[row.RouteID] {
			t.Fatal("duplicate or changed charge", row)
		}
		if row.RouteID == a.RouteID && (row.CardBrand != "visa" || row.PaymentMethod != "card") {
			t.Fatal("missing card observation", row)
		}
		verified[row.RouteID] = true
	}
	if !verified[a.RouteID] || !verified[b.RouteID] {
		t.Fatal(verified)
	}

	// A later source can introduce ambiguity after a valid earlier close.
	another := BankEntry{ID: "another-bank-credit", Trace: xa.Trace, Currency: xa.Currency, Beneficiary: xa.Beneficiary, Source: digest, Amount: xa.Net}
	req(bank, "POST", "/v1/bank-entries", another, 201, nil)
	var historical payoutRecord
	req(merchant, "GET", a.path+xa.ID, nil, 200, &historical)
	if historical.ApprovedBy == "" || !historical.FundingMatchAmbiguous {
		t.Fatal("missing original approval or later conflict", historical)
	}
	req(merchant, "GET", b.path+xb.ID, nil, 200, &historical)
	if historical.FundingMatchAmbiguous {
		t.Fatal("unrelated trace contaminated", historical)
	}

}

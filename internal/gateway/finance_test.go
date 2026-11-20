package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/spanner"
	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func financeTestID(t *testing.T) string {
	t.Helper()
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(id[:])
}

func TestFinancialControlsPostgres(t *testing.T) {
	dsn := os.Getenv("DAKIM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable DAKIM_TEST_DATABASE_URL")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "finance_" + financeTestID(t)
	q := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+q); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, e := admin.Exec(context.Background(), "DROP DATABASE "+q); e != nil {
			t.Error(e)
		}
	}()
	cfg := admin.Config().Copy()
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = gwstore.InstallPostgres(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store, err := gwstore.NewPostgres(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	qualifyFinancialControls(t, store)
	t.Run("independent processor namespaces", func(t *testing.T) { qualifyFinancialRoutes(t, store) })
}

func TestFinancialControlsSpanner(t *testing.T) {
	name := os.Getenv("DAKIM_TEST_SPANNER_DATABASE")
	if name == "" || os.Getenv("SPANNER_EMULATOR_HOST") == "" {
		t.Skip("requires local Spanner emulator")
	}
	ctx := t.Context()
	client, err := spanner.NewClient(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	store, err := gwstore.NewSpanner(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	qualifyFinancialControls(t, store)
	t.Run("independent processor namespaces", func(t *testing.T) { qualifyFinancialRoutes(t, store) })
}

func qualifyFinancialControls(t *testing.T, store gwstore.Store) {
	s := New(store, nil)
	ctx := t.Context()
	tenant := "mimi-" + financeTestID(t)
	principal := func(role Role, subject string) Principal {
		return Principal{Merchant: tenant, Account: "provider-account", Currency: "USD", Beneficiary: "beneficiary", Role: role, Subject: subject}
	}
	merchant := principal(MerchantRole, "retail")
	statement := principal(StatementRole, "partner-ingest")
	bank := principal(BankRole, "bank-ingest")
	maker := principal(FinanceRole, "finance-maker")
	checker := principal(ReviewerRole, "finance-checker")
	mux := http.NewServeMux()
	s.registerFinance(mux)
	req := financeRequest(t, mux)
	digest := strings.Repeat("a", 64)
	command := func(id, kind, parent string, amount int64) railcontract.Command {
		return railcontract.Command{ID: id, Merchant: tenant, Account: merchant.Account, Kind: kind, Parent: parent, Amount: amount, Currency: "USD", Order: "one"}
	}
	post := func(c railcontract.Command, ref string) error {
		return store.Update(ctx, func(tx gwstore.Tx) error { return postConfirmedMovement(tx, c, ref, "", s.Now()) })
	}
	invalid := journalEntry{Merchant: tenant, Account: merchant.Account, Currency: "USD", Kind: "capture", Reference: "bad",
		Legs: [2]journalLeg{{Account: processorClearing, Debit: 5}, {Account: merchantProceeds, Credit: 4}}}
	if err := store.Update(ctx, func(tx gwstore.Tx) error { return appendPosting(tx, invalid) }); err == nil {
		t.Fatal("unbalanced posting was accepted")
	}
	capture := command("c1", "charge", "", 11000)
	refund := command("f1", "refund", "c1", 1000)
	// Concurrent observations of the same movement produce one immutable entry.
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- post(capture, "cap") }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := post(refund, "ref"); err != nil {
		t.Fatal(err)
	}
	if err := post(capture, "changed"); err == nil {
		t.Fatal("changed operation reference accepted")
	}
	var snapshot struct {
		Entries []journalEntry `json:"entries"`
		Through int64          `json:"through_sequence"`
		Next    int64          `json:"next_after"`
	}
	req(merchant, "GET", "/v1/journal?limit=1", nil, 200, &snapshot)
	if snapshot.Through != 2 || len(snapshot.Entries) != 1 || snapshot.Next != 1 {
		t.Fatal(snapshot)
	}

	base := SettlementStatement{ID: "batch", Trace: "trace", Beneficiary: "beneficiary", Currency: "USD", Net: 9746, Source: digest, Lines: []StatementLine{
		{LineID: "1", Kind: "capture", Reference: "cap", Amount: 11000}, {LineID: "2", Kind: "refund", Reference: "ref", Amount: -1000},
		{LineID: "3", Kind: "fee", Reference: "processing", Parent: "cap", Amount: -254}}}
	wrong := base
	wrong.Lines = append([]StatementLine(nil), base.Lines...)
	wrong.Lines[0].Reference = "missing"
	req(statement, "POST", "/v1/settlement-statements", wrong, 409, nil)
	req(merchant, "POST", "/v1/settlement-statements", base, 403, nil)
	req(statement, "POST", "/v1/settlement-statements", base, 201, nil)
	req(statement, "POST", "/v1/settlement-statements", base, 200, nil)
	req(maker, "POST", "/v1/payouts/batch/prepare-close", map[string]string{"bank_entry_id": "missing"}, 409, nil)
	reallocated := base
	reallocated.ID = "other"
	req(statement, "POST", "/v1/settlement-statements", reallocated, 409, nil)
	b := BankEntry{ID: "posted", Trace: base.Trace, Beneficiary: base.Beneficiary, Currency: "USD", Amount: 9746, Source: strings.Repeat("b", 64)}
	// Changing only the beneficiary grant cannot import to, read or close the original book.
	shifted := maker
	shifted.Beneficiary = "other-alias"
	req(shifted, "GET", "/v1/journal", nil, 403, nil)
	req(shifted, "GET", "/v1/payouts/batch", nil, 403, nil)
	req(shifted, "POST", "/v1/payouts/batch/prepare-close", map[string]string{"bank_entry_id": b.ID}, 403, nil)
	shifted.Role = BankRole
	shiftedEntry := b
	shiftedEntry.Beneficiary = shifted.Beneficiary
	req(shifted, "POST", "/v1/bank-entries", shiftedEntry, 403, nil)
	req(bank, "POST", "/v1/bank-entries", b, 201, nil)
	req(bank, "POST", "/v1/bank-entries", b, 200, nil)
	bad := b
	bad.Amount++
	req(bank, "POST", "/v1/bank-entries", bad, 409, nil)
	var prepared payoutRecord
	req(maker, "POST", "/v1/payouts/batch/prepare-close", map[string]string{"bank_entry_id": b.ID}, 200, &prepared)
	if prepared.Status != "prepared" || prepared.Proposal == "" {
		t.Fatal(prepared)
	}
	req(principal(ReviewerRole, maker.Subject), "POST", "/v1/payouts/batch/approve-close", map[string]string{"proposal_id": prepared.Proposal}, 403, nil)
	req(checker, "POST", "/v1/payouts/batch/approve-close", map[string]string{"proposal_id": "wrong"}, 409, nil)
	out := payoutRecord{}
	req(checker, "POST", "/v1/payouts/batch/approve-close", map[string]string{"proposal_id": prepared.Proposal}, 200, &out)
	req(checker, "POST", "/v1/payouts/batch/approve-close", map[string]string{"proposal_id": prepared.Proposal}, 200, nil)
	if out.Status != "matched" || out.ApprovedBy != checker.Subject {
		t.Fatal(out)
	}
	req(merchant, "GET", "/v1/journal?after=1&through=2", nil, 200, &snapshot)
	if len(snapshot.Entries) != 1 || snapshot.Entries[0].Sequence != 2 {
		t.Fatal(snapshot)
	}

	// A separately itemized $20 dispute and posted bank debit survive any later close.
	dispute := SettlementStatement{ID: "later", Trace: "trace-debit", Beneficiary: "beneficiary", Currency: "USD", Net: -2000, Source: digest,
		Lines: []StatementLine{{LineID: "4", Kind: "chargeback", Reference: "dispute", Parent: "cap", Amount: -2000}}}
	req(statement, "POST", "/v1/settlement-statements", dispute, 201, nil)
	b.ID, b.Trace, b.Amount = "debit", dispute.Trace, -2000
	req(bank, "POST", "/v1/bank-entries", b, 201, nil)
	req(maker, "POST", "/v1/payouts/later/prepare-close", map[string]string{"bank_entry_id": "debit"}, 200, &prepared)
	req(checker, "POST", "/v1/payouts/later/approve-close", map[string]string{"proposal_id": prepared.Proposal}, 200, nil)
	req(merchant, "GET", "/v1/journal", nil, 200, &snapshot)
	balances := map[string]int64{}
	for _, e := range snapshot.Entries {
		if e.Legs[0].Debit != e.Legs[1].Credit || e.Legs[0].Debit <= 0 || e.Legs[0].Credit != 0 || e.Legs[1].Debit != 0 {
			t.Fatalf("unbalanced %+v", e)
		}
		for _, l := range e.Legs {
			balances[l.Account] += l.Debit - l.Credit
		}
	}
	if len(snapshot.Entries) != 10 || balances[bankObserved] != 7746 || balances[merchantProceeds] != -7746 || balances[processorClearing] != 0 || balances[fundingInTransit] != 0 || balances[bankUnallocated] != 0 {
		t.Fatal(snapshot.Entries, balances)
	}
	// A return of the original $97.46 independently posts at the bank. The
	// initial close remains valid history and re-funding requires a new close.
	returned := BankEntry{ID: "return-bank", Trace: "return-trace", Beneficiary: merchant.Beneficiary, Currency: "USD", Amount: -9746, Source: digest, ReturnOf: "posted"}
	missing := returned
	missing.ReturnOf = "unknown"
	req(bank, "POST", "/v1/bank-entries", missing, 409, nil)
	req(bank, "POST", "/v1/bank-entries", returned, 201, nil)
	req(bank, "POST", "/v1/bank-entries", returned, 200, nil)
	req(merchant, "GET", "/v1/payouts/batch", nil, 200, &out)
	if out.Status != "matched" || out.Returned != 9746 || out.ProcessorRecredit != 0 {
		t.Fatal("bank return erased prior close or inferred a recredit", out)
	}
	reissue := SettlementStatement{ID: "replacement", Trace: "replacement-trace", Beneficiary: merchant.Beneficiary, Currency: "USD", Net: 9746, Source: digest,
		Lines: []StatementLine{{LineID: "replacement-line", Kind: "return_recredit", Reference: "processor-recredit", Parent: returned.ID, Amount: 9746}}}
	invented := reissue
	invented.Lines = append([]StatementLine(nil), reissue.Lines...)
	invented.Lines[0].Parent = "not-a-return"
	req(statement, "POST", "/v1/settlement-statements", invented, 409, nil)
	req(statement, "POST", "/v1/settlement-statements", reissue, 201, nil)
	overcredit := reissue
	overcredit.ID = "another-replacement"
	overcredit.Net = 1
	overcredit.Lines = []StatementLine{{LineID: "unknown-line", Kind: "return_recredit", Reference: "over", Parent: returned.ID, Amount: 1}}
	req(statement, "POST", "/v1/settlement-statements", overcredit, 409, nil)
	req(merchant, "GET", "/v1/payouts/batch", nil, 200, &out)
	if out.BankEntry != "posted" || out.Returned != 9746 || out.ProcessorRecredit != 9746 {
		t.Fatal(out)
	}
	req(maker, "POST", "/v1/payouts/replacement/prepare-close", map[string]string{"bank_entry_id": "posted"}, 409, nil)
	replacement := BankEntry{ID: "bank-replacement", Trace: reissue.Trace, Beneficiary: merchant.Beneficiary, Currency: "USD", Amount: 9746, Source: digest}
	req(bank, "POST", "/v1/bank-entries", replacement, 201, nil)
	req(maker, "POST", "/v1/payouts/replacement/prepare-close", map[string]string{"bank_entry_id": replacement.ID}, 200, &prepared)
	req(checker, "POST", "/v1/payouts/replacement/approve-close", map[string]string{"proposal_id": prepared.Proposal}, 200, &out)
	req(merchant, "GET", "/v1/journal", nil, 200, &snapshot)
	balances = map[string]int64{}
	for _, e := range snapshot.Entries {
		for _, leg := range e.Legs {
			balances[leg.Account] += leg.Debit - leg.Credit
		}
	}
	if balances[bankObserved] != 7746 || balances[merchantProceeds] != -7746 || balances[bankReturnSuspense] != 0 || balances[fundingInTransit] != 0 || balances[processorClearing] != 0 || balances[bankUnallocated] != 0 {
		t.Fatal("return and replacement did not reconcile", balances)
	}
	checkFinancialHold := func(command railcontract.Command, ref string, want bool) {
		t.Helper()
		err := store.View(ctx, func(tx gwstore.Tx) error {
			held, e := releaseFinancialHold(tx, command, ref)
			if e == nil && held != want {
				t.Fatalf("release hold on %s got %t want %t", ref, held, want)
			}
			return e
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	checkFinancialHold(capture, "cap", true)
	undo := SettlementStatement{ID: "dispute-returned", Trace: "trace-r", Beneficiary: "beneficiary", Currency: "USD", Net: 2000, Source: digest,
		Lines: []StatementLine{{LineID: "representment-line", Kind: "chargeback_reversal", Reference: "won", Parent: "dispute", Amount: 2000}}}
	req(statement, "POST", "/v1/settlement-statements", undo, 201, nil)
	checkFinancialHold(capture, "cap", false)
	foreign := merchant
	foreign.Merchant += "-foreign"
	req(foreign, "GET", "/v1/payouts/batch", nil, 404, nil)
	req(foreign, "GET", "/v1/journal", nil, 200, &snapshot)
	if len(snapshot.Entries) != 0 {
		t.Fatal("unscoped journal")
	}

	over := SettlementStatement{ID: "duplicate", Trace: "dupe-trace", Beneficiary: "beneficiary", Currency: "USD", Net: 11000, Source: digest,
		Lines: []StatementLine{{LineID: "5", Kind: "additional_capture", Reference: "second-debit", Parent: "cap", Amount: 11000}}}
	req(statement, "POST", "/v1/settlement-statements", over, 201, &out)
	checkFinancialHold(capture, "cap", true)
	if len(out.Exceptions) != 1 {
		t.Fatal("excess capture not exposed", out)
	}
	b.ID, b.Trace, b.Amount = "dupe-bank", over.Trace, over.Net
	req(bank, "POST", "/v1/bank-entries", b, 201, nil)
	req(maker, "POST", "/v1/payouts/duplicate/prepare-close", map[string]string{"bank_entry_id": b.ID}, 409, nil)
	// The independent processor reports restitution in two later installments.
	// The first installment cannot clear the hold. A full refund permits the
	// original payout to be prepared, preserving both debits and the case ID.
	for index, charge := range []int64{5000, 6000} {
		suffix := strconv.Itoa(index)
		restore := SettlementStatement{ID: "return-" + suffix, Trace: "ret-trace-" + suffix, Beneficiary: "beneficiary", Currency: "USD", Net: -charge, Source: digest,
			Lines: []StatementLine{{LineID: "return-line-" + suffix, Kind: "refund_excess_capture", Reference: "real-refund-" + suffix, Parent: "second-debit", Amount: -charge}}}
		req(statement, "POST", "/v1/settlement-statements", restore, 201, nil)
		if index == 0 {
			req(maker, "POST", "/v1/payouts/duplicate/prepare-close", map[string]string{"bank_entry_id": b.ID}, 409, nil)
		}
	}
	req(maker, "POST", "/v1/payouts/duplicate/prepare-close", map[string]string{"bank_entry_id": b.ID}, 200, &prepared)
	req(checker, "POST", "/v1/payouts/duplicate/approve-close", map[string]string{"proposal_id": prepared.Proposal}, 200, &out)
	checkFinancialHold(capture, "cap", false)
	if out.Status != "matched" || len(out.Exceptions) != 1 {
		t.Fatal("lost historical external duplicate identity")
	}
	sourceRefund := SettlementStatement{ID: "provider-refund", Trace: "trace-sr", Beneficiary: "beneficiary", Currency: "USD", Net: -300, Source: digest,
		Lines: []StatementLine{{LineID: "source-refund-line", Kind: "source_refund", Reference: "external-refund", Parent: "cap", Amount: -300}}}
	req(statement, "POST", "/v1/settlement-statements", sourceRefund, 201, nil)
	checkFinancialHold(capture, "cap", true)
	// Two independently posted rows with identical matching terms cannot be
	// chosen arbitrarily, even if the operator supplies a particular row ID.
	if err := post(command("new", "charge", "", 1500), "cap-2"); err != nil {
		t.Fatal(err)
	}
	amb := SettlementStatement{ID: "amb", Trace: "amb-trace", Beneficiary: "beneficiary", Currency: "USD", Net: 1500, Source: digest,
		Lines: []StatementLine{{LineID: "6", Kind: "capture", Reference: "cap-2", Amount: 1500}}}
	req(statement, "POST", "/v1/settlement-statements", amb, 201, nil)
	b.ID, b.Trace, b.Amount = "amb1", amb.Trace, amb.Net
	req(bank, "POST", "/v1/bank-entries", b, 201, nil)
	b.ID = "amb2"
	req(bank, "POST", "/v1/bank-entries", b, 201, nil)
	req(maker, "POST", "/v1/payouts/amb/prepare-close", map[string]string{"bank_entry_id": "amb1"}, 409, nil)
	// The converse ambiguity matters as well: one bank row cannot select
	// between two unrelated payouts with equal signed amounts and traces.
	for i := 0; i < 2; i++ {
		ref := "equal-ref-" + strconv.Itoa(i)
		id := "equal-batch-" + strconv.Itoa(i)
		if err := post(command("equal-op-"+strconv.Itoa(i), "charge", "", 102), "cap-"+ref); err != nil {
			t.Fatal(err)
		}
		x := SettlementStatement{ID: id, Trace: "equal-trace", Currency: "USD", Beneficiary: merchant.Beneficiary, Net: 102, Source: digest,
			Lines: []StatementLine{{LineID: ref, Kind: "capture", Reference: "cap-" + ref, Amount: 102}}}
		req(statement, "POST", "/v1/settlement-statements", x, 201, nil)
	}
	b.ID, b.Trace, b.Amount = "equal-bank", "equal-trace", 102
	req(bank, "POST", "/v1/bank-entries", b, 201, nil)
	req(maker, "POST", "/v1/payouts/equal-batch-0/prepare-close", map[string]string{"bank_entry_id": b.ID}, 409, nil)
}

func TestFinanceWireAndDirection(t *testing.T) {
	bad := []string{`{"payout_id":"a","Net_minor":0}`, `{"lines":[{"reference":"c","reference":"b"}]}`,
		`{"net_minor":null}`, `{"net_minor":0}{}`, `{"unexpected":"x","net_minor":0}`, `{}`}
	for _, in := range bad {
		r := httptest.NewRequest("POST", "/", strings.NewReader(in))
		var x SettlementStatement
		if err := financeJSON(r, &x, "net_minor"); err == nil {
			t.Fatal("invalid input accepted", in)
		}
	}
	legs, err := financialLegs(fundingInTransit, processorClearing, -9746)
	if err != nil || legs[0].Account != processorClearing || legs[1].Account != fundingInTransit ||
		legs[0].Debit != 9746 || legs[1].Credit != 9746 {
		t.Fatal(legs, err)
	}
}

func financeRequest(t *testing.T, mux http.Handler) func(Principal, string, string, any, int, any) {
	t.Helper()
	return func(p Principal, method, path string, in any, want int, result any) {
		t.Helper()
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		if w.Code != want {
			t.Fatalf("%s %s got %d, want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		if result != nil {
			reflect.ValueOf(result).Elem().SetZero()
			if err = json.Unmarshal(w.Body.Bytes(), result); err != nil {
				t.Fatal(err)
			}
		}
	}
}

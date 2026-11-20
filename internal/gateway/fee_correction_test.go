package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const correctionEndpoint = "/v2/settlement-fee-corrections"
const correctionPayouts = "/v1/routes/route-first/accounts/same-acquirer-local-mid/payouts/"
const correctionExportPath = "/v2/routes/route-first/accounts/same-acquirer-local-mid/payouts/corrected-day/confirmed-fee-correction"
const correctionGolden = "../../integrations/mimi-fixtures/confirmed-fee-correction-v1.json"

var correctionTestTime = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

type correctionRail struct{ *routeTestRail }

func (r correctionRail) InspectRelease(ctx context.Context, c railcontract.Command) (railcontract.ReleaseInventory, error) {
	facts, err := r.routeTestRail.InspectRelease(ctx, c)
	facts.ObservedAt = correctionTestTime
	return facts, err
}

type correctionFixture struct {
	merchant, statement, bank, maker, reviewer Principal
	handler                                    http.Handler
}

func newCorrectionFixture(s *Service) correctionFixture {
	merchant := engineTestPrincipal()
	merchant.Merchant, merchant.Subject = "mimi-synthetic", "retail"
	role := func(r Role, subject string) Principal {
		p := merchant
		p.Role, p.Subject = r, subject
		return p
	}
	statement := role(StatementRole, "synthetic-statement")
	statement.StatementRoute, statement.StatementProviderAccount = "route-first", "same-acquirer-local-mid"
	pub := correctionTestKey().Public().(ed25519.PublicKey)
	statement.FeeCorrectionKeys = map[string]string{"fixture-key": hex.EncodeToString(pub)}
	return correctionFixture{merchant, statement, role(BankRole, "synthetic-bank"), role(FinanceRole, "fixture-maker"), role(ReviewerRole, "fixture-checker"), s.Handler()}
}

func correctionTestKey() ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("public test vector for the synthetic dakimpay fee correction fixture"))
	return ed25519.NewKeyFromSeed(seed[:])
}

func signCorrection(terms FeeCorrectionTerms) SignedFeeCorrection {
	in := SignedFeeCorrection{KeyID: "fixture-key", Terms: terms}
	in.Signature = hex.EncodeToString(ed25519.Sign(correctionTestKey(), in.SigningBytes()))
	return in
}

func correctionRequest(m http.Handler, who Principal, method, path string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Idempotency-Key", "fixture-checkout")
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), who)))
	return w
}

func (f correctionFixture) request(t *testing.T, who Principal, method, path string, in any, code int) []byte {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	res := correctionRequest(f.handler, who, method, path, raw)
	if res.Code != code {
		t.Fatalf("%s %s: expected %d, got %d: %s", method, path, code, res.Code, res.Body.Bytes())
	}
	return res.Body.Bytes()
}

func correctionDecode[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func (f correctionFixture) concurrent(t *testing.T, p Principal, path string, in any, wantCreated int) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 6)
	for i := 0; i < cap(results); i++ {
		go func() { <-start; results <- correctionRequest(f.handler, p, "POST", path, raw) }()
	}
	close(start)
	created := 0
	for i := 0; i < cap(results); i++ {
		r := <-results
		switch r.Code {
		case 201:
			created++
		case 200:
		default:
			t.Errorf("%s concurrent response %d: %s", path, r.Code, r.Body.Bytes())
		}
	}
	if created != wantCreated {
		t.Fatalf("%s: %d created, want %d", path, created, wantCreated)
	}
}

func (f correctionFixture) journal(t *testing.T) []journalEntry {
	t.Helper()
	return correctionDecode[struct{ Entries []journalEntry }](t, f.request(t, f.merchant, "GET", "/v1/journal", nil, 200)).Entries
}

func correctionBank(id, trace string, amount int64, digest string) BankEntry {
	return BankEntry{ID: id, Trace: trace, Beneficiary: "merchant-bank", Currency: "USD", Amount: amount, Source: strings.Repeat(digest, 64)}
}

func (f correctionFixture) closeOriginal(t *testing.T, original SettlementStatement, bank BankEntry) payoutRecord {
	t.Helper()
	f.request(t, f.statement, "POST", "/v1/settlement-statements", original, 201)
	f.request(t, f.bank, "POST", "/v1/bank-entries", bank, 201)
	path := correctionPayouts + original.ID
	prepared := correctionDecode[payoutRecord](t, f.request(t, f.maker, "POST", path+"/prepare-close", map[string]string{"bank_entry_id": bank.ID}, 200))
	closed := correctionDecode[payoutRecord](t, f.request(t, f.reviewer, "POST", path+"/approve-close", map[string]string{"proposal_id": prepared.Proposal}, 200))
	if closed.Status != "matched" {
		t.Fatal("expected bank-confirmed source payout")
	}
	return closed
}

// The first child runs real v2 admission and the worker against an in-memory
// acquirer. The second starts with an empty process and the same PostgreSQL book.
// Both children exit normally. Only inter-process reference values are saved here.
type correctionCheckpoint struct {
	PID                                   int
	Envelope                              SignedFeeCorrection
	Proposal                              string
	OldClose, OldJournal, Export, Payment []byte
	JournalCount                          int
}

func TestBookedFeeCorrectionPostgres(t *testing.T) {
	dsn := os.Getenv("DAKIM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires dedicated local synthetic PostgreSQL")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg.ConnConfig.Host) || cfg.ConnConfig.TLSConfig != nil {
		t.Fatal("fee correction fixture requires a local PostgreSQL Unix socket")
	}
	ctx := t.Context()
	phase := os.Getenv("DAKIM_FEE_TEST_PHASE")
	if phase == "" {
		admin, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer admin.Close()
		name := "fee_fixture_" + financeTestID(t)
		id := pgx.Identifier{name}.Sanitize()
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+id); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := admin.Exec(context.Background(), "DROP DATABASE "+id); err != nil {
				t.Error(err)
			}
		}()
		dir := t.TempDir()
		bin, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		for _, stage := range []string{"settle", "replay"} {
			cmd := exec.CommandContext(ctx, bin, "-test.run=^TestBookedFeeCorrectionPostgres$")
			cmd.Env = append(os.Environ(), "DAKIM_FEE_TEST_PHASE="+stage, "DAKIM_FEE_TEST_DB="+name, "DAKIM_FEE_TEST_DIR="+dir)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v\n%s", stage, err, out)
			}
		}
		return
	}

	cfg.ConnConfig.Database = os.Getenv("DAKIM_FEE_TEST_DB")
	if ok, _ := regexp.MatchString(`^fee_fixture_[0-9a-f]{24}$`, cfg.ConnConfig.Database); !ok {
		t.Fatal("invalid disposable database")
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if phase == "settle" {
		if err := gwstore.InstallPostgres(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	store, err := gwstore.NewPostgres(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	rail := correctionRail{newRouteTestRail()}
	s := New(store, rail)
	s.Now = func() time.Time { return correctionTestTime }
	f := newCorrectionFixture(s)
	file := filepath.Join(os.Getenv("DAKIM_FEE_TEST_DIR"), "checkpoint.json")
	switch phase {
	case "settle":
		s.SelectRoute = func(c railcontract.Command) (railcontract.Command, error) { return setTestRoute(c, "first"), nil }
		cp := f.settleCorrection(t, s, rail)
		data, err := json.Marshal(cp)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
	case "replay":
		s.Now = func() time.Time { return correctionTestTime.Add(48 * time.Hour) }
		s.SelectRoute = func(c railcontract.Command) (railcontract.Command, error) {
			t.Fatal("replayed payment or correction consulted new route preference")
			return c, nil
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		cp := correctionDecode[correctionCheckpoint](t, data)
		f.replayCorrection(t, cp)
		t.Run("separate fee debit and bank return", func(t *testing.T) { f.debitCorrection(t, cp) })
		if len(rail.submissions) != 0 {
			t.Fatal("restart replay submitted another payment")
		}
	default:
		t.Fatal("unexpected fixture phase")
	}
}

func fixtureSale() engineIntentInput {
	in := testIntent()
	in.Order, in.Allocation, in.PaymentMethod, in.Amount = "mimi-order-001", "tender-card-001", "visa", 11000
	return in
}

func (f correctionFixture) settleCorrection(t *testing.T, s *Service, rail correctionRail) correctionCheckpoint {
	t.Helper()
	// This child process is private to this non-parallel test. Only v2
	// admission consumes the known 16 bytes; production ID generation is unchanged.
	sale := func() engineView {
		defaultReader := rand.Reader
		rand.Reader = io.MultiReader(strings.NewReader("fixture-sale-001"), defaultReader)
		defer func() { rand.Reader = defaultReader }()
		return correctionDecode[engineView](t, f.request(t, f.merchant, "POST", "/v2/payment-intents", fixtureSale(), 201))
	}()
	if err := s.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	paymentPath := "/v2/payment-intents/" + sale.ID
	view := correctionDecode[engineView](t, f.request(t, f.merchant, "GET", paymentPath, nil, 200))
	if view.ID != "pi_666978747572652d73616c652d303031" || view.Status != "captured" || view.Reference != "native-001" || view.ConnectorID != "first" {
		t.Fatal("incorrect captured sale", view)
	}
	if release := readRouteRelease(t, s, f.merchant, sale.ID); !release.Eligible || release.Payment.Operation != sale.ID {
		t.Fatal("initial retail release", release)
	}
	refund := correctionDecode[engineView](t, f.request(t, f.merchant, "POST", paymentPath+"/refunds", engineRefundInput{BusinessID: "retail-refund", Amount: 1000}, 201))
	if err := s.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	rail.reversals = []railcontract.Fact{rail.facts[refund.ID]}
	if release := readRouteRelease(t, s, f.merchant, sale.ID); release.Eligible || release.Reason != "original_refunded" {
		t.Fatal("refunded sale acquired another retail release", release)
	}
	route := FinancialRoute{RouteID: view.RouteID, ProviderAccount: view.ProviderAccount}
	original := SettlementStatement{FinancialRoute: route, ID: "original-day", Trace: "original-trace", Beneficiary: f.merchant.Beneficiary, Currency: "USD", Net: 9746, Source: strings.Repeat("a", 64), Lines: []StatementLine{
		{LineID: "01-capture", Kind: "capture", Reference: view.Reference, Amount: 11000},
		{LineID: "02-refund", Kind: "refund", Reference: "native-return-001", Amount: -1000},
		{LineID: "03-fee", Kind: "fee", Reference: "interchange-001", Parent: view.Reference, Amount: -127},
		{LineID: "04-fee", Kind: "fee", Reference: "assessment-001", Parent: view.Reference, Amount: -127},
	}}
	closed := f.closeOriginal(t, original, correctionBank("original-bank-credit", original.Trace, original.Net, "b"))
	terms := FeeCorrectionTerms{FinancialRoute: route, SourceSubject: f.statement.Subject, Merchant: f.merchant.Merchant, Account: f.merchant.Account, Currency: "USD", Beneficiary: f.merchant.Beneficiary, ConnectorID: view.ConnectorID, Operation: sale.ID, Order: view.Order, Allocation: view.Allocation, Capture: view.Reference, OriginalPayout: original.ID, OriginalProposal: closed.Proposal, OriginalSource: original.Source, OriginalLine: "03-fee", OriginalFee: "interchange-001", OriginalAmount: -127, ID: "fee-correction-001", Payout: "corrected-day", Trace: "correction-trace", Source: strings.Repeat("c", 64), Delta: 27}
	cp := correctionCheckpoint{PID: os.Getpid(), Envelope: signCorrection(terms),
		OldClose: f.request(t, f.merchant, "GET", correctionPayouts+original.ID, nil, 200),
		Payment:  f.request(t, f.merchant, "GET", paymentPath, nil, 200)}
	// A separately paid fee can reference this capture. Its payout never
	// settled the capture and therefore cannot stand in for the original close.
	separate := SettlementStatement{FinancialRoute: route, ID: "fee-only-day", Trace: "other-trace", Beneficiary: original.Beneficiary, Currency: "USD", Source: strings.Repeat("e", 64), Net: -127,
		Lines: []StatementLine{{LineID: "other-fee-line", Kind: "fee", Reference: "other-fee", Parent: view.Reference, Amount: -127}}}
	other := f.closeOriginal(t, separate, correctionBank("other-bank-debit", separate.Trace, separate.Net, "f"))
	s.Now = func() time.Time { return correctionTestTime.Add(24 * time.Hour) }
	otherTerms := terms
	otherTerms.OriginalPayout, otherTerms.OriginalProposal, otherTerms.OriginalSource = other.ID, other.Proposal, other.Source
	otherTerms.OriginalLine, otherTerms.OriginalFee = "other-fee-line", "other-fee"
	f.request(t, f.statement, "POST", correctionEndpoint, signCorrection(otherTerms), 409)

	for _, wrong := range []struct {
		field  string
		change func(*FeeCorrectionTerms)
	}{
		{"fee line with same total", func(v *FeeCorrectionTerms) { v.OriginalLine = "04-fee" }},
		{"different native fee with same total", func(v *FeeCorrectionTerms) { v.OriginalFee = "assessment-001" }},
		{"processor", func(v *FeeCorrectionTerms) { v.ConnectorID = "second" }},
		{"tender", func(v *FeeCorrectionTerms) { v.Allocation = "another-tender" }},
	} {
		t.Run(wrong.field, func(t *testing.T) {
			x := terms
			wrong.change(&x)
			f.request(t, f.statement, "POST", correctionEndpoint, signCorrection(x), 409)
		})
	}
	tampered := cp.Envelope
	tampered.Terms.Delta++
	f.request(t, f.statement, "POST", correctionEndpoint, tampered, 403)
	f.request(t, f.merchant, "GET", correctionPayouts+terms.Payout, nil, 404)
	f.request(t, f.merchant, "GET", correctionExportPath, nil, 404)
	rows := f.journal(t)
	cp.OldJournal = f.request(t, f.merchant, "GET", fmt.Sprintf("/v1/journal?through=%d", len(rows)), nil, 200)
	f.concurrent(t, f.statement, correctionEndpoint, cp.Envelope, 1)
	changed := terms
	changed.Delta++
	f.request(t, f.statement, "POST", correctionEndpoint, signCorrection(changed), 409)
	alias := terms
	alias.ID, alias.Payout, alias.Trace = "duplicate-credit", "duplicate-day", "duplicate-trace"
	f.request(t, f.statement, "POST", correctionEndpoint, signCorrection(alias), 409)
	legacyAlias := SettlementStatement{FinancialRoute: route, ID: "legacy-alias", Trace: "legacy-trace", Beneficiary: terms.Beneficiary,
		Currency: terms.Currency, Net: terms.Delta, Source: terms.Source,
		Lines: []StatementLine{{LineID: "legacy-line", Kind: "fee_rebate", Reference: terms.ID, Parent: terms.Capture, Amount: terms.Delta}}}
	f.request(t, f.statement, "POST", "/v1/settlement-statements", legacyAlias, 409)
	correctedPath := correctionPayouts + terms.Payout
	correctBank := correctionBank("correction-bank-credit", terms.Trace, terms.Delta, "d")
	useBank := map[string]string{"bank_entry_id": correctBank.ID}
	f.request(t, f.maker, "POST", correctedPath+"/prepare-close", useBank, 409)
	f.request(t, f.merchant, "GET", correctionExportPath, nil, 409)
	if got := f.journal(t); len(got) != len(rows) {
		t.Fatal("correction staging posted to the journal", got)
	}
	// Equal signed value alone is insufficient; this separate observed
	// bank movement has the wrong trace.
	wrongBank := correctionBank("different-bank-credit", "wrong-trace", terms.Delta, "0")
	f.request(t, f.bank, "POST", "/v1/bank-entries", wrongBank, 201)
	f.request(t, f.maker, "POST", correctedPath+"/prepare-close", map[string]string{"bank_entry_id": wrongBank.ID}, 409)
	f.request(t, f.bank, "POST", "/v1/bank-entries", correctBank, 201)
	prepared := correctionDecode[payoutRecord](t, f.request(t, f.maker, "POST", correctedPath+"/prepare-close", useBank, 200))
	cp.Proposal = prepared.Proposal
	approval := map[string]string{"proposal_id": cp.Proposal}
	for _, actor := range []Principal{f.maker, f.statement, f.bank} {
		self := f.reviewer
		self.Subject = actor.Subject
		f.request(t, self, "POST", correctedPath+"/approve-close", approval, 403)
	}
	f.request(t, f.merchant, "GET", correctionExportPath, nil, 409)
	for _, j := range f.journal(t) {
		if j.Kind == "fee_correction" || j.Reference == terms.Payout {
			t.Fatal("financial effect before independent approval", j)
		}
	}
	f.concurrent(t, f.reviewer, correctedPath+"/approve-close", approval, 0)
	cp.Export = f.request(t, f.merchant, "GET", correctionExportPath, nil, 200)
	got := correctionDecode[FeeCorrectionExport](t, cp.Export)
	if got.Schema != correctionExportSchema || got.Terms != terms || got.RevisedAmount != -100 || got.BankEntry != correctBank.ID || got.BankSource != correctBank.Source || got.Proposal != prepared.Proposal || got.OriginalBankEntry != closed.BankEntry || got.ProcessorLedger != rail.ledger["first"] || got.CardBrand != "visa" || got.ApprovedBy != f.reviewer.Subject || got.PreparedBy != f.maker.Subject || !financialDigest.MatchString(got.PostingID) {
		t.Fatal("confirmed export differs from source evidence", got)
	}
	journal := f.journal(t)
	var captures, corrections, remittances, closes int
	for _, row := range journal {
		switch {
		case row.Kind == "capture":
			captures++
		case row.Kind == "fee_correction":
			corrections++
			if row.Reference != terms.ID || row.Operation != sale.ID || row.Order != terms.Order || row.ConnectorID != terms.ConnectorID || row.Parent != terms.OriginalFee || row.Sequence != got.Sequence || row.Legs != ([2]journalLeg{{Account: processorClearing, Debit: 27}, {Account: merchantProceeds, Credit: 27}}) {
				t.Fatal("incorrect correction journal posting", row)
			}
		case row.Reference == terms.Payout && row.Kind == "remittance":
			remittances++
		case row.Reference == terms.Payout && row.Kind == "settlement_close":
			closes++
		}
	}
	if captures != 1 || corrections != 1 || remittances != 1 || closes != 1 || len(rail.submissions) != 2 {
		t.Fatalf("captures=%d corrections=%d remittances=%d closes=%d submitted=%d", captures, corrections, remittances, closes, len(rail.submissions))
	}
	if !bytes.Equal(cp.OldClose, f.request(t, f.merchant, "GET", correctionPayouts+terms.OriginalPayout, nil, 200)) || !bytes.Equal(cp.Payment, f.request(t, f.merchant, "GET", paymentPath, nil, 200)) {
		t.Fatal("correction changed original retail or payout record")
	}
	cp.JournalCount = len(journal)
	if path := os.Getenv("DAKIM_FEE_FIXTURE_OUTPUT"); path != "" {
		if !filepath.IsAbs(path) {
			t.Fatal("fixture output must be an absolute path")
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.Write(cp.Export)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("writing confirmed export: write=%v close=%v", writeErr, closeErr)
		}
	} else {
		golden, err := os.ReadFile(correctionGolden)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(golden, cp.Export) {
			t.Fatal("generated confirmed HTTP export differs from committed synthetic fixture")
		}
	}
	return cp
}

func (f correctionFixture) replayCorrection(t *testing.T, cp correctionCheckpoint) {
	t.Helper()
	if cp.PID == os.Getpid() {
		t.Fatal("replay reused original process")
	}
	terms := cp.Envelope.Terms
	f.request(t, f.statement, "POST", correctionEndpoint, cp.Envelope, 200)
	bank := correctionBank("correction-bank-credit", terms.Trace, terms.Delta, "d")
	f.request(t, f.bank, "POST", "/v1/bank-entries", bank, 200)
	f.request(t, f.reviewer, "POST", correctionPayouts+terms.Payout+"/approve-close", map[string]string{"proposal_id": cp.Proposal}, 200)
	if !bytes.Equal(cp.Export, f.request(t, f.merchant, "GET", correctionExportPath, nil, 200)) {
		t.Fatal("confirmed export changed across process restart")
	}
	replay := f.request(t, f.merchant, "POST", "/v2/payment-intents", fixtureSale(), 200)
	if !bytes.Equal(cp.Payment, replay) {
		t.Fatal("replay changed immutable retail payment")
	}
	history := correctionDecode[struct {
		Through int64 `json:"through_sequence"`
	}](t, cp.OldJournal)
	if !bytes.Equal(cp.OldJournal, f.request(t, f.merchant, "GET", fmt.Sprintf("/v1/journal?through=%d", history.Through), nil, 200)) || !bytes.Equal(cp.OldClose, f.request(t, f.merchant, "GET", correctionPayouts+terms.OriginalPayout, nil, 200)) || len(f.journal(t)) != cp.JournalCount {
		t.Fatal("history or journal count changed on replay")
	}
	// Later contradictory bank evidence changes current eligibility and leaves
	// the original settlement and previously booked correction untouched.
	bank.ID = "late-equal-bank-credit"
	bank.Source = strings.Repeat("7", 64)
	f.request(t, f.bank, "POST", "/v1/bank-entries", bank, 201)
	f.request(t, f.merchant, "GET", correctionExportPath, nil, 409)
	if !bytes.Equal(cp.OldClose, f.request(t, f.merchant, "GET", correctionPayouts+terms.OriginalPayout, nil, 200)) || len(f.journal(t)) != cp.JournalCount+1 {
		t.Fatal("late contradictory evidence rewrote financial history")
	}
}

// A different original fee receives an increased charge and a later bank return.
// Check each signed ledger leg and the historical approval after the return.
func (f correctionFixture) debitCorrection(t *testing.T, cp correctionCheckpoint) {
	terms := cp.Envelope.Terms
	terms.OriginalLine, terms.OriginalFee = "04-fee", "assessment-001"
	terms.ID, terms.Payout, terms.Trace = "debit-002", "debit-day", "debit-trace"
	terms.Delta, terms.Source = -12, strings.Repeat("8", 64)
	f.request(t, f.statement, "POST", correctionEndpoint, signCorrection(terms), 201)
	bank := correctionBank("fee-debit-bank", terms.Trace, -12, "9")
	f.request(t, f.bank, "POST", "/v1/bank-entries", bank, 201)
	path := correctionPayouts + terms.Payout
	prepared := correctionDecode[payoutRecord](t, f.request(t, f.maker, "POST", path+"/prepare-close", map[string]string{"bank_entry_id": bank.ID}, 200))
	f.request(t, f.reviewer, "POST", path+"/approve-close", map[string]string{"proposal_id": prepared.Proposal}, 200)
	exportPath := "/v2/routes/" + terms.RouteID + "/accounts/" + terms.ProviderAccount + "/payouts/" + terms.Payout + "/confirmed-fee-correction"
	posted := correctionDecode[FeeCorrectionExport](t, f.request(t, f.merchant, "GET", exportPath, nil, 200))
	if posted.Terms != terms || posted.RevisedAmount != -139 || posted.OriginalBankEntry == posted.BankEntry {
		t.Fatalf("incorrect fee debit: %+v", posted)
	}
	rows := f.journal(t)
	expected := map[string][2]journalLeg{
		"fee_correction":   {{Account: merchantProceeds, Debit: 12}, {Account: processorClearing, Credit: 12}},
		"remittance":       {{Account: processorClearing, Debit: 12}, {Account: fundingInTransit, Credit: 12}},
		"bank_posting":     {{Account: bankUnallocated, Debit: 12}, {Account: bankObserved, Credit: 12}},
		"settlement_close": {{Account: fundingInTransit, Debit: 12}, {Account: bankUnallocated, Credit: 12}},
	}
	for _, row := range rows {
		if row.Reference != terms.ID && row.Reference != terms.Payout && row.Reference != bank.ID {
			continue
		}
		if row.Legs != expected[row.Kind] {
			t.Fatalf("wrong debit legs in %s: %+v", row.Kind, row.Legs)
		}
		delete(expected, row.Kind)
	}
	if len(expected) != 0 {
		t.Fatalf("missing debit postings: %+v", expected)
	}
	asOf := fmt.Sprintf("/v1/journal?through=%d", len(rows))
	history := f.request(t, f.merchant, "GET", asOf, nil, 200)
	reversal := correctionBank("fee-debit-return", "return-trace", 12, "6")
	reversal.ReturnOf = bank.ID
	f.request(t, f.bank, "POST", "/v1/bank-entries", reversal, 201)
	f.request(t, f.merchant, "GET", exportPath, nil, 409)
	payout := correctionDecode[payoutRecord](t, f.request(t, f.merchant, "GET", path, nil, 200))
	if payout.Status != "matched" || payout.Proposal != posted.Proposal || payout.ApprovedBy != posted.ApprovedBy || payout.Returned != -12 {
		t.Fatal("bank return changed the historical debit approval", payout)
	}
	if !bytes.Equal(history, f.request(t, f.merchant, "GET", asOf, nil, 200)) ||
		!bytes.Equal(cp.OldClose, f.request(t, f.merchant, "GET", correctionPayouts+terms.OriginalPayout, nil, 200)) {
		t.Fatal("bank return changed the previous journal or original close")
	}
}

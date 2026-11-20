package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func engineTestStore(t *testing.T) gwstore.Store {
	t.Helper()
	dsn := os.Getenv("DAKIM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set DAKIM_TEST_DATABASE_URL for gateway transaction tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	id, err := engineID("gw_engine_")
	if err != nil {
		t.Fatal(err)
	}
	name := pgx.Identifier{id}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+name); err != nil {
			t.Error(err)
		}
	})
	cfg := admin.Config().Copy()
	cfg.ConnConfig.Database = id
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = gwstore.InstallPostgres(ctx, pool); err != nil {
		t.Fatal(err)
	}
	s, err := gwstore.NewPostgres(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type engineTestRail struct {
	facts       map[string]railcontract.Fact
	submits     int
	ledger      string
	unavailable bool
}

func (r *engineTestRail) Identity(ctx context.Context, c railcontract.Command) (string, error) {
	if r.ledger == "" {
		return strings.Repeat("a", 64), nil
	}
	return r.ledger, nil
}
func (r *engineTestRail) Submit(_ context.Context, c railcontract.Command) error {
	r.submits++
	if _, ok := r.facts[c.ID]; !ok {
		f := railcontract.Fact{Command: c, State: "captured", Reference: "movement_" + c.ID}
		if c.Kind == "refund" {
			f.State = "refunded"
		}
		r.facts[c.ID] = f
	}
	return errors.New("reply lost after processor commit")
}
func (r *engineTestRail) Lookup(_ context.Context, c railcontract.Command) (railcontract.Fact, bool, error) {
	if r.unavailable {
		return railcontract.Fact{}, false, errors.New("intermediary 404")
	}
	f, ok := r.facts[c.ID]
	return f, ok, nil
}
func engineTestPrincipal() Principal {
	return Principal{Merchant: "merchant-x", Account: "acq-x", Subject: "merchant-server-credential", Beneficiary: "merchant-bank", Role: MerchantRole, MaxAmount: 1000000, Currency: "USD", Outlets: map[string]bool{"shop": true}, Terminals: map[string]string{"lane1": "shop"}}
}
func testIntent() engineIntentInput {
	return engineIntentInput{Order: "order", Allocation: "tender-1", Outlet: "shop", Channel: "terminal", Terminal: "lane1", Amount: 900, Currency: "USD"}
}

func TestEngineRetainsOriginalAcrossAmbiguityAndStaleWorker(t *testing.T) {
	ctx := context.Background()
	rail := &engineTestRail{facts: map[string]railcontract.Fact{}, unavailable: true}
	s := New(engineTestStore(t), rail)
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	p := engineTestPrincipal()
	created, _, err := s.createIntent(ctx, p, "create", testIntent())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if rail.submits != 0 {
		t.Fatal("lookup failure caused dispatch")
	}
	read := func() engineOperation {
		var op engineOperation
		err := s.Store.View(ctx, func(tx gwstore.Tx) error {
			var e error
			op, e = engineGet(tx, p.Merchant, created.Command.ID)
			return e
		})
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	if read().Status != "unknown" {
		t.Fatal("ambiguous outcome not visible")
	}
	rail.unavailable = false
	now = now.Add(2 * time.Second)
	first, ok, err := s.claimWork(ctx)
	if err != nil || !ok {
		t.Fatal("first lease", ok, err)
	}
	// Crash after committing the charge at the separate processor, before
	// durable result application. Another worker recreates the original state.
	_ = rail.Submit(ctx, first.Command)
	now = now.Add(engineLease + time.Second)
	second, ok, err := s.claimWork(ctx)
	if err != nil || !ok || first.Ticket == second.Ticket {
		t.Fatal("replacement lease", ok, err)
	}
	if err = s.processWork(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err = s.finishWork(ctx, first, "declined", "", railcontract.Fact{}); err != nil {
		t.Fatal(err)
	}
	op := read()
	if op.Status != "captured" || op.Reference != "movement_"+op.Command.ID || rail.submits != 1 {
		t.Fatalf("late worker changed verified charge: %+v", op)
	}
	replayed, dup, err := s.createIntent(ctx, p, "create", testIntent())
	if err != nil || !dup || replayed.Command.ID != created.Command.ID || replayed.Status != "captured" {
		t.Fatal("original replay", dup, err)
	}
	// A second attempt with a new idempotency key cannot consume even a
	// different allocation against the same fully paid basket.
	other := testIntent()
	other.Allocation = "tender-2"
	if _, _, err = s.createIntent(ctx, p, "other", other); err == nil {
		t.Fatal("per-order total exceeded")
	}
}
func TestEngineConcurrentRefundReservationsAndOriginalReplay(t *testing.T) {
	ctx := context.Background()
	rail := &engineTestRail{facts: map[string]railcontract.Fact{}}
	s := New(engineTestStore(t), rail)
	p := engineTestPrincipal()
	parent, _, err := s.createIntent(ctx, p, "collect", testIntent())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		op  engineOperation
		err error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, ref := range []string{"return-A", "return-B"} {
		wg.Add(1)
		go func(ref string) {
			defer wg.Done()
			op, _, err := s.createRefund(ctx, p, ref, parent.Command.ID, engineRefundInput{Amount: 600, BusinessID: ref})
			results <- outcome{op, err}
		}(ref)
	}
	wg.Wait()
	close(results)
	var winner engineOperation
	var conflicts int
	for r := range results {
		var e engineError
		switch {
		case r.err == nil:
			winner = r.op
		case errors.As(r.err, &e) && e.status == 409:
			conflicts++
		default:
			t.Fatal(r.err)
		}
	}
	if winner.Command.ID == "" || conflicts != 1 {
		t.Fatal("refund capacity oversubscribed")
	}
	replay, dup, err := s.createRefund(ctx, p, "new-http-key", parent.Command.ID, engineRefundInput{Amount: 600, BusinessID: winner.RefundKey})
	if err != nil || !dup || replay.Command.ID != winner.Command.ID {
		t.Fatal("business refund reference replay", err)
	}
	if err = s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var original engineOperation
	if err = s.Store.View(ctx, func(tx gwstore.Tx) error {
		var e error
		original, e = engineGet(tx, p.Merchant, parent.Command.ID)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if original.Refunded != 600 || original.Reserved != 0 || rail.submits != 2 {
		t.Fatalf("incorrect refund state %+v", original)
	}
}
func TestEngineRejectsAmbiguousEconomicJSON(t *testing.T) {
	for _, body := range []string{`{"amount_minor":2,"amount_minor":1}`, `{"AMOUNT_MINOR":4}`, `{"currency":null}`, `{"card_number":"unsupported"}`, `{"amount_minor":14}{}`} {
		r := httptest.NewRequest(http.MethodPost, "/v2/payment-intents", strings.NewReader(body))
		var in engineIntentInput
		if err := engineDecode(r, &in); err == nil {
			t.Errorf("accepted ambiguous request %q", body)
		}
	}
}

func TestReviewerCanOnlyInquireOnQuarantinedOriginal(t *testing.T) {
	ctx := context.Background()
	rail := &engineTestRail{facts: map[string]railcontract.Fact{}}
	s := New(engineTestStore(t), rail)
	merchant := engineTestPrincipal()
	original, _, err := s.createIntent(ctx, merchant, "present", testIntent())
	if err != nil {
		t.Fatal(err)
	}
	id := original.Command.ID
	wrong := original.Command
	wrong.Merchant = "unrelated-merchant"
	rail.facts[id] = railcontract.Fact{Command: wrong, State: "captured", Reference: "other-tenant-private-reference"}
	if err = s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	read := func() engineOperation {
		t.Helper()
		var op engineOperation
		err := s.Store.View(ctx, func(tx gwstore.Tx) error { var err error; op, err = engineGet(tx, merchant.Merchant, id); return err })
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	quarantined := read()
	if quarantined.Status != "review" || quarantined.Reference != "" {
		t.Fatalf("unbound observation accepted %+v", quarantined)
	}
	reviewer := merchant
	reviewer.Role = ReviewerRole
	reviewer.Subject = "reviewer-one"
	h := s.Handler()
	request := func(p Principal, method, path, body, key string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r = r.WithContext(WithPrincipal(r.Context(), p))
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	base := "/v2/operations/" + id
	facts := request(reviewer, "GET", base+"/evidence", "", "")
	if facts.Code != 200 || !strings.Contains(facts.Body.String(), "normalized_sha256") || strings.Contains(facts.Body.String(), wrong.Merchant) || strings.Contains(facts.Body.String(), "other-tenant-private-reference") {
		t.Fatal("untrusted data in evidence", facts.Code, facts.Body.String())
	}
	makeBody := func(v int64) string {
		return fmt.Sprintf(`{"expected_version":%d,"reason_code":"partner_evidence_available"}`, v)
	}
	body := makeBody(quarantined.Version)
	if w := request(merchant, "POST", base+"/recheck", body, "retry"); w.Code != 403 {
		t.Fatal("merchant initiated release", w.Code)
	}
	if w := request(reviewer, "POST", base+"/recheck", body, "retry"); w.Code != 202 {
		t.Fatal("reviewer inquiry", w.Code, w.Body.String())
	}
	delete(rail.facts, id)
	if err = s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.Status != "review" || rail.submits != 0 {
		t.Fatal("inquiry became a second financial instruction", got, rail.submits)
	}
	if w := request(reviewer, "POST", base+"/recheck", body, "retry"); w.Code != 200 {
		t.Fatal("recheck idempotency", w.Code)
	}
	rail.facts[id] = railcontract.Fact{Command: original.Command, State: "captured", Reference: "movement_real"}
	if w := request(reviewer, "POST", base+"/recheck", makeBody(read().Version), "retry2"); w.Code != 202 {
		t.Fatal("second reviewer inquiry", w.Code, w.Body.String())
	}
	if err = s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.Status != "captured" || got.Reference != "movement_real" || rail.submits != 0 {
		t.Fatal("verified original not recovered", got)
	}
	facts = request(reviewer, "GET", base+"/evidence", "", "")
	if !strings.Contains(facts.Body.String(), "reviewer-one") || !strings.Contains(facts.Body.String(), "movement_real") {
		t.Fatal("incomplete independent history", facts.Body.String())
	}
}

type engineReleaseRail struct {
	*engineTestRail
	inventory railcontract.ReleaseInventory
	err       error
}

func (r *engineReleaseRail) InspectRelease(_ context.Context, _ railcontract.Command) (railcontract.ReleaseInventory, error) {
	return r.inventory, r.err
}
func TestReleaseRequiresFreshSourceAndCurrentRefundCapacity(t *testing.T) {
	ctx := context.Background()
	p := engineTestPrincipal()
	rail := &engineReleaseRail{engineTestRail: &engineTestRail{facts: map[string]railcontract.Fact{}}}
	s := New(engineTestStore(t), rail)
	now := time.Now().UTC()
	s.Now = func() time.Time { return now }
	op, _, err := s.createIntent(ctx, p, "sale", testIntent())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	check := func(p Principal, wantCode int, wantReason string, wantEligible bool) {
		t.Helper()
		r := httptest.NewRequest("GET", "/v2/payment-intents/"+op.Command.ID+"/release-evidence", nil)
		r = r.WithContext(WithPrincipal(r.Context(), p))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != wantCode {
			t.Fatalf("HTTP %d want %d: %s", w.Code, wantCode, w.Body.String())
		}
		if wantCode != 200 {
			return
		}
		var decision engineReleaseDecision
		if err := json.NewDecoder(w.Body).Decode(&decision); err != nil {
			t.Fatal(err)
		}
		if decision.Eligible != wantEligible || decision.Reason != wantReason {
			t.Fatalf("release %+v", decision)
		}
		if wantEligible && (!decision.Complete || decision.ProcessorAt == nil || decision.GatewayAt.Before(*decision.ProcessorAt)) {
			t.Fatalf("unbounded release cutoff %+v", decision)
		}
	}
	rail.inventory = railcontract.ReleaseInventory{Fact: rail.facts[op.Command.ID], Complete: true, ObservedAt: now, Refunds: []railcontract.Fact{}, DisputeStatus: "none"}
	check(p, 200, "verified_capture", true)
	another := p
	another.Merchant = "other"
	check(another, 404, "", false)
	rail.inventory.ObservedAt = now.Add(-time.Minute)
	check(p, 200, "processor_inventory_stale", false)
	rail.inventory.ObservedAt = now
	rail.inventory.DisputeStatus = "open"
	check(p, 200, "dispute_not_clear", false)
	rail.inventory.DisputeStatus = "none"
	rail.inventory.Refunds = nil
	check(p, 200, "processor_inventory_incomplete", false)
	rail.inventory.Refunds = []railcontract.Fact{}
	rail.err = errors.New("feed outage")
	check(p, 503, "", false)
	rail.err = nil
	refund, _, err := s.createRefund(ctx, p, "request-refund", op.Command.ID, engineRefundInput{Amount: 100, BusinessID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	check(p, 200, "refund_pending", false)
	if err = s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	check(p, 200, "refund_evidence_differs", false)
	rail.inventory.Refunds = []railcontract.Fact{rail.facts[refund.Command.ID]}
	check(p, 200, "original_refunded", false)
}

func TestReplacedLedgerCannotReissueAnAmbiguousIntent(t *testing.T) {
	ctx := t.Context()
	p := engineTestPrincipal()
	first := &engineTestRail{facts: map[string]railcontract.Fact{}}
	s := New(engineTestStore(t), first)
	sale := testIntent()
	op, _, err := s.createIntent(ctx, p, "sale", sale)
	if err != nil {
		t.Fatal(err)
	}
	// The original processor committed, then the gateway lost its response.
	_ = first.Submit(railcontract.WithProcessorIdentity(ctx, op.Ledger), op.Command)
	replacement := &engineTestRail{ledger: strings.Repeat("b", 64), facts: map[string]railcontract.Fact{}}
	s = New(s.Store, replacement)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	read := func(id string) engineOperation {
		t.Helper()
		var out engineOperation
		err := s.Store.View(ctx, func(tx gwstore.Tx) error { var e error; out, e = engineGet(tx, p.Merchant, id); return e })
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := read(op.Command.ID); got.Status != "review" || got.Reason != "processor_ledger_identity_unverified" || replacement.submits != 0 {
		t.Fatalf("replacement was trusted: %+v", got)
	}
	if replay, dup, err := s.createIntent(ctx, p, "sale", sale); err != nil || !dup || replay.Command.ID != op.Command.ID {
		t.Fatal("original request replay lost", dup, err)
	}
	second := sale
	second.Order = "new-order"
	second.Channel = "hosted"
	second.Terminal = ""
	if _, _, err := s.createIntent(ctx, p, "second", second); err == nil || err.Error() != "processor_ledger_changed" {
		t.Fatal("merchant program silently changed ledgers", err)
	}
	p.Beneficiary = "another-bank"
	if _, _, err := s.createIntent(ctx, p, "different-bank", second); err == nil || err.Error() != "program_enrollment_changed" {
		t.Fatal("beneficiary silently changed", err)
	}
	p = engineTestPrincipal()
	s.Rail = first
	if err := s.Store.Update(ctx, func(tx gwstore.Tx) error {
		row, err := engineGet(tx, p.Merchant, op.Command.ID)
		if err != nil {
			return err
		}
		row.Ledger = ""
		row.Status = "unknown"
		row.InquiryOnly = false
		if err = engineEnqueue(tx, &row, s.Now()); err != nil {
			return err
		}
		if err = engineSave(tx, row); err != nil {
			return err
		}
		return tx.Delete(engineKey("processor-enrollment", p.Merchant, p.Account))
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.createIntent(ctx, p, "legacy-new", second); err == nil || err.Error() != "legacy_processor_enrollment_required" {
		t.Fatal("legacy ledger automatically rebound", err)
	}
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := read(op.Command.ID); got.Status != "review" || got.Reference != "" || first.submits != 1 {
		t.Fatalf("unbound legacy original posted %+v", got)
	}
}

func TestRefundDispatchRechecksCaptureFinancialHold(t *testing.T) {
	ctx := t.Context()
	p := engineTestPrincipal()
	rail := &engineTestRail{facts: map[string]railcontract.Fact{}}
	s := New(engineTestStore(t), rail)
	now := time.Now().UTC()
	s.Now = func() time.Time { return now }
	payment, _, err := s.createIntent(ctx, p, "sale", testIntent())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	refund, _, err := s.createRefund(ctx, p, "refund", payment.Command.ID, engineRefundInput{BusinessID: "returned-item", Amount: 300})
	if err != nil {
		t.Fatal(err)
	}
	prefix := bookPrefix(p.Merchant, p.Account, p.Currency)
	ref := rail.facts[payment.Command.ID].Reference
	if err := s.Store.Update(ctx, func(tx gwstore.Tx) error { return openReleaseCase(tx, prefix, "dispute", "d1", ref, 900) }); err != nil {
		t.Fatal(err)
	}
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if rail.submits != 1 {
		t.Fatal("new refund submitted after chargeback arose", rail.submits)
	}
	err = s.Store.View(ctx, func(tx gwstore.Tx) error {
		child, err := engineGet(tx, p.Merchant, refund.Command.ID)
		if err != nil {
			return err
		}
		parent, err := engineGet(tx, p.Merchant, payment.Command.ID)
		if err != nil {
			return err
		}
		if child.Reason != "original_financial_exception" || child.Ticket == "" || parent.Reserved != 300 || parent.Refunded != 0 {
			t.Fatalf("exposure lost while held %+v %+v", child, parent)
		}
		var admission engineEvidence
		ok, err := gwstore.GetJSON(tx, engineEvidencePrefix(child.Command)+"admission", &admission)
		want, _ := GrantFingerprint(p)
		if !ok || admission.Actor != p.Subject || admission.Role != MerchantRole || admission.GrantDigest != want {
			t.Fatalf("admission missing attribution %+v", admission)
		}
		public, _ := json.Marshal(enginePresent(child))
		if strings.Contains(string(public), p.Subject) || strings.Contains(string(public), want) {
			t.Fatalf("private origin disclosed %s", public)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Store.Update(ctx, func(tx gwstore.Tx) error { return restoreReleaseCase(tx, prefix, "dispute", "d1", 900) }); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err = s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if rail.submits != 2 || rail.facts[refund.Command.ID].State != "refunded" {
		t.Fatal("original refund failed to recover")
	}
}

type engineAbsentAfterSubmit struct{ *engineTestRail }

func (r *engineAbsentAfterSubmit) Submit(_ context.Context, _ railcontract.Command) error {
	r.submits++
	return errors.New("response lost")
}

func TestAbsenceAfterEgressNeverAuthorizesAnotherSubmission(t *testing.T) {
	for _, scenario := range []string{"crash-before-send", "same-ledger-rollback", "immediate-source-absence"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			p := engineTestPrincipal()
			rail := &engineTestRail{facts: map[string]railcontract.Fact{}}
			s := New(engineTestStore(t), rail)
			now := time.Now().UTC()
			s.Now = func() time.Time { return now }
			op, _, err := s.createIntent(ctx, p, "order", testIntent())
			if err != nil {
				t.Fatal(err)
			}
			effects := 0
			if scenario == "immediate-source-absence" {
				s.Rail = &engineAbsentAfterSubmit{rail}
				effects = 1
			} else {
				claimed, ok, err := s.claimWork(ctx)
				if err != nil || !ok {
					t.Fatal("original not claimed", err)
				}
				reason, ok, err := s.commitDispatch(ctx, claimed)
				if err != nil || !ok || reason != "" {
					t.Fatal("egress not committed", reason, ok, err)
				}
				if _, second, err := s.commitDispatch(ctx, claimed); err != nil || second {
					t.Fatal("same worker acquired a second permit", err)
				}
				if scenario == "same-ledger-rollback" {
					_ = rail.Submit(railcontract.WithProcessorIdentity(ctx, claimed.Ledger), claimed.Command)
					delete(rail.facts, claimed.Command.ID)
					effects = 1
				}
				now = now.Add(engineLease + time.Second)
			}
			if err := s.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			err = s.Store.View(ctx, func(tx gwstore.Tx) error {
				result, err := engineGet(tx, p.Merchant, op.Command.ID)
				if err != nil {
					return err
				}
				if !result.EgressCommitted || !result.InquiryOnly || result.Status != "review" || result.Ticket != "" || result.Reason != "previously_dispatched_operation_missing" || rail.submits != effects {
					t.Fatalf("unsafe second submission or release %+v submits=%d", result, rail.submits)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCurrentProgramDriftBlocksNewEgressAndPrivilegedReports(t *testing.T) {
	ctx := t.Context()
	p := engineTestPrincipal()
	rail := &engineTestRail{facts: map[string]railcontract.Fact{}}
	s := New(engineTestStore(t), rail)
	now := time.Now().UTC()
	s.Now = func() time.Time { return now }
	fresh, _, err := s.createIntent(ctx, p, "new", testIntent())
	if err != nil {
		t.Fatal(err)
	}
	in := testIntent()
	in.Order = "other"
	in.Terminal = ""
	in.Channel = "hosted"
	inFlight, _, err := s.createIntent(ctx, p, "in-flight", in)
	if err != nil {
		t.Fatal(err)
	}
	_ = rail.Submit(railcontract.WithProcessorIdentity(ctx, inFlight.Ledger), inFlight.Command)
	current := p
	current.Beneficiary = "different-bank"
	s.CurrentProgram = func(string) (Principal, bool) { return current, true }
	if err = s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertStatus := func(id, state, reason string) {
		t.Helper()
		err := s.Store.View(ctx, func(tx gwstore.Tx) error {
			op, err := engineGet(tx, p.Merchant, id)
			if err != nil {
				return err
			}
			if op.Status != state || op.Reason != reason {
				t.Fatalf("%s: got %+v", id, op)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	assertStatus(fresh.Command.ID, "queued", "program_enrollment_changed")
	assertStatus(inFlight.Command.ID, "captured", "")
	if rail.submits != 1 {
		t.Fatal("fresh money sent despite beneficiary drift")
	}
	h := s.Handler()
	request := func(p Principal, method, path, body string, code int, want string) {
		t.Helper()
		r := httptest.NewRequest(method, "/v2/operations/"+inFlight.Command.ID+path, strings.NewReader(body))
		r = r.WithContext(WithPrincipal(r.Context(), p))
		r.Header.Set("Idempotency-Key", "review1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != code || !strings.Contains(w.Body.String(), want) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	changedReviewer := current
	changedReviewer.Role = ReviewerRole
	request(changedReviewer, "GET", "/evidence", "", 409, "program_enrollment_changed")
	request(changedReviewer, "POST", "/recheck", `{"expected_version":2,"reason_code":"partner_evidence_available"}`, 409, "program_enrollment_changed")
	request(p, "GET", "", "", 200, "captured")
	current = p
	now = now.Add(time.Minute)
	if err = s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertStatus(fresh.Command.ID, "captured", "")
}

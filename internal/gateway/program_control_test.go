package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/spanner"
	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
)

func TestProgramControl(t *testing.T) {
	t.Run("postgres", func(t *testing.T) { verifyProgramControl(t, engineTestStore(t)) })
	t.Run("spanner", func(t *testing.T) {
		name := os.Getenv("DAKIM_TEST_SPANNER_DATABASE")
		if name == "" || os.Getenv("SPANNER_EMULATOR_HOST") == "" {
			t.Skip("requires disposable Spanner emulator")
		}
		client, err := spanner.NewClient(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		store, err := gwstore.NewSpanner(t.Context(), client)
		if err != nil {
			t.Fatal(err)
		}
		verifyProgramControl(t, store)
	})
}

func verifyProgramControl(t *testing.T, store gwstore.Store) {
	t.Helper()
	ctx := t.Context()
	rail := &engineTestRail{facts: map[string]railcontract.Fact{}}
	s := New(store, rail)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	merchant, err := engineID("merchant_")
	if err != nil {
		t.Fatal(err)
	}
	maker := Principal{Role: FinanceRole, Merchant: merchant, Subject: "maker"}
	checker := Principal{Role: ReviewerRole, Merchant: merchant, Subject: "checker"}
	self := Principal{Role: ReviewerRole, Merchant: merchant, Subject: "maker"}
	customer := Principal{Role: MerchantRole, Merchant: merchant, Subject: "register"}
	other := Principal{Role: ReviewerRole, Merchant: merchant + "2", Subject: "checker2"}
	h := s.Handler()
	call := func(p Principal, method, path, key string, body any, want int) map[string]any {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, "/v2/program/sales"+path, bytes.NewReader(raw))
		req.Header.Set("Idempotency-Key", key)
		req = req.WithContext(WithPrincipal(req.Context(), p))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s as %s: got %d %s, want %d", method, path, p.Subject, w.Code, w.Body, want)
		}
		result := map[string]any{}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	open := func(want bool) {
		t.Helper()
		got, err := s.saleDispatchOpen(ctx, merchant)
		if err != nil || got != want {
			t.Fatalf("dispatch open %v %v, want %v", got, err, want)
		}
		err = store.View(ctx, func(tx gwstore.Tx) error { return checkSaleAdmission(tx, merchant) })
		if (err == nil) != want || (!want && err.Error() != "new_sales_suspended") {
			t.Fatalf("admission %v, want open %v", err, want)
		}
	}
	input := map[string]any{"reason": "investigating unknown captures"}
	call(customer, "POST", "/freeze", "inc", input, 403)
	call(other, "GET", "", "", nil, 200)
	open(true)
	buyer := engineTestPrincipal()
	buyer.Merchant = merchant
	a := testIntent()
	opA, _, err := s.createIntent(ctx, buyer, "a", a)
	if err != nil {
		t.Fatal(err)
	}
	b := a
	b.Order, b.Channel, b.Terminal = "existing-sale", "hosted", ""
	opB, _, err := s.createIntent(ctx, buyer, "b", b)
	if err != nil {
		t.Fatal(err)
	}
	// The independent rail has committed B, but its success reply was lost.
	// A is also admitted locally and has not reached the rail.
	_ = rail.Submit(ctx, opB.Command)
	freeze := call(maker, "POST", "/freeze", "inc", input, 200)
	if freeze["suspension_version"] != float64(1) {
		t.Fatalf("first freeze %v", freeze)
	}
	open(false)
	retry, replayed, err := s.createIntent(ctx, buyer, "a", a)
	if err != nil || !replayed || retry.Command.ID != opA.Command.ID {
		t.Fatalf("frozen retry %v %v", replayed, err)
	}
	c := b
	c.Order = "blocked-new-sale"
	if _, _, err := s.createIntent(ctx, buyer, "c", c); err == nil || err.Error() != "new_sales_suspended" {
		t.Fatalf("freeze admitted new sale: %v", err)
	}
	// Simulate gateway restart before recovery. Control is persisted, existing
	// evidence can be accepted, and unseen A cannot leave the process.
	restarted := New(store, rail)
	restarted.Now = s.Now
	if err := restarted.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if rail.submits != 1 {
		t.Fatalf("freeze allowed a queued charge to reach rail: %d submits", rail.submits)
	}
	ref, _, err := restarted.createRefund(ctx, buyer, "return1", opB.Command.ID, engineRefundInput{Amount: 60, BusinessID: "return1"})
	if err != nil {
		t.Fatalf("original captured sale unavailable for return: %v", err)
	}
	if err := restarted.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if rail.submits != 2 || rail.facts[ref.Command.ID].State != "refunded" {
		t.Fatalf("return did not proceed under freeze")
	}
	call(maker, "POST", "/freeze", "inc", input, 200)
	call(maker, "POST", "/freeze", "inc", map[string]string{"reason": "other"}, 409)
	if call(customer, "GET", "", "", nil, 200)["audit_sequence"] != float64(1) {
		t.Fatal("freeze replay created history")
	}
	propose := func(version int, key string) map[string]any {
		return call(maker, "POST", "/resume-proposals", key, map[string]any{"suspension_version": version, "reason": "partner evidence verified"}, 200)
	}
	approval := func(proposal map[string]any) map[string]any {
		return map[string]any{
			"proposal_id": proposal["proposal_id"], "proposal_digest": proposal["proposal_digest"], "suspension_version": proposal["suspension_version"],
		}
	}
	first := propose(1, "request1")
	call(other, "POST", "/resume-approvals", "review", approval(first), 404)
	call(self, "POST", "/resume-approvals", "review", approval(first), 403)
	bad := approval(first)
	bad["proposal_digest"] = strings.Repeat("0", 64)
	call(checker, "POST", "/resume-approvals", "review", bad, 409)
	call(checker, "POST", "/freeze", "incident2", input, 200)
	call(checker, "POST", "/resume-approvals", "review", approval(first), 409)
	second := propose(2, "request2")
	now = now.Add(16 * time.Minute)
	call(checker, "POST", "/resume-approvals", "review", approval(second), 409)
	third := propose(2, "request3")
	open(false)
	receipt := call(checker, "POST", "/resume-approvals", "review", approval(third), 200)
	open(true)
	if err := restarted.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if rail.submits != 3 || rail.facts[opA.Command.ID].State != "captured" {
		t.Fatalf("frozen queued payment did not resume on original ID")
	}
	call(maker, "POST", "/freeze", "inc", input, 200)
	open(true)
	call(checker, "POST", "/freeze", "incident3", input, 200)
	call(checker, "POST", "/resume-approvals", "review", approval(third), 200)
	open(false)
	call(checker, "POST", "/resume-approvals", "review-again", approval(third), 409)
	call(customer, "GET", "/audit", "", nil, 403)
	log := call(maker, "GET", "/audit", "", nil, 200)
	events := log["events"].([]any)
	if len(events) != 7 || receipt["sequence"] != float64(6) {
		t.Fatalf("unexpected event count or receipt: %v %v", log, receipt)
	}
	if log2 := call(maker, "GET", "/audit?after=6", "", nil, 200); len(log2["events"].([]any)) != 1 || log2["next_after"] != float64(7) {
		t.Fatalf("audit cursor %v", log2)
	}
	for i, raw := range events {
		e := raw.(map[string]any)
		if fmt.Sprint(e["merchant_id"]) != merchant || e["sequence"] != float64(i+1) {
			t.Fatalf("wrong scope/order: %v", e)
		}
	}
	if report := call(other, "GET", "/audit", "", nil, 200); len(report["events"].([]any)) != 0 {
		t.Fatalf("tenant leak %v", report)
	}
}

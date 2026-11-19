package simulator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type testStore struct {
	acquirer, bank *httptest.Server
	aq, bk         *pgxpool.Pool
	dropBankReply  atomic.Bool
}

const serviceKey = "processor-and-bank-read"
const postKey = "independent-bank-operator"
const instructKey = "acquirer-funding"
const approveKey = "operator-checkout"

func scopedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DAKIM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set DAKIM_TEST_DATABASE_URL to an isolated PostgreSQL database to run simulator integration tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	random, err := tokenID()
	if err != nil {
		t.Fatal(err)
	}
	name := "test_sim_" + strings.ToLower(strings.ReplaceAll(random[:16], "-", "_"))
	id := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "create schema "+id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(ctx, "drop schema "+id+" cascade")
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
func newTestStore(t *testing.T) *testStore {
	t.Helper()
	s := &testStore{aq: scopedPool(t), bk: scopedPool(t)}
	bank, err := BankWithKeys(t.Context(), s.bk, serviceKey, instructKey, postKey)
	if err != nil {
		t.Fatal(err)
	}
	s.bank = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bank/instructions" && s.dropBankReply.CompareAndSwap(true, false) {
			bank.ServeHTTP(httptest.NewRecorder(), r)
			panic(http.ErrAbortHandler)
		}
		bank.ServeHTTP(w, r)
	}))
	t.Cleanup(s.bank.Close)
	s.acquirer = httptest.NewUnstartedServer(nil)
	origin := "http://" + s.acquirer.Listener.Addr().String()
	h, err := AcquirerWithKeys(t.Context(), s.aq, origin, s.bank.URL, serviceKey, instructKey, approveKey)
	if err != nil {
		t.Fatal(err)
	}
	s.acquirer.Config.Handler = h
	s.acquirer.Start()
	t.Cleanup(s.acquirer.Close)
	return s
}

func request(method, target, key string, body any) (int, []byte, error) {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, target, payload)
	if err != nil {
		return 0, nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer r.Body.Close()
	resp, err := io.ReadAll(r.Body)
	return r.StatusCode, resp, err
}
func call(t *testing.T, method, addr, key string, data any, code int, result any) []byte {
	t.Helper()
	status, raw, err := request(method, addr, key, data)
	if err != nil || status != code {
		t.Fatalf("%s %s returned %d (%v), want %d: %s", method, addr, status, err, code, raw)
	}
	if result != nil {
		if err := json.Unmarshal(raw, result); err != nil {
			t.Fatalf("malformed %q: %v", raw, err)
		}
	}
	return raw
}
func count(t *testing.T, p *pgxpool.Pool, query string, want int) {
	t.Helper()
	var got int
	if err := p.QueryRow(t.Context(), query).Scan(&got); err != nil || got != want {
		t.Fatalf("%s returned %d (%v), want %d", query, got, err, want)
	}
}
func TestIndependentSaleRecoveryAndBankPosting(t *testing.T) {
	s := newTestStore(t)
	ac := s.acquirer.URL
	bank := s.bank.URL
	terminal := chargeRequest{Operation: "terminal-attempt", Merchant: merchant, Order: "MIMI-GUIDED-TERMINAL", Channel: "terminal", Amount: 4200, Currency: currency, Brand: "visa", Terminal: "MIMI-01", LoseResponse: true}
	code, _, err := request("POST", ac+"/sim/charges", serviceKey, terminal)
	if err == nil || code != 0 {
		t.Fatalf("expected client connection to end with no payment response; got %d / %v", code, err)
	}
	var initial, repeat charge
	call(t, "GET", ac+"/sim/charges/"+terminal.Operation, serviceKey, nil, 200, &initial)
	call(t, "POST", ac+"/sim/charges", serviceKey, terminal, 200, &repeat)
	if initial != repeat || initial.Status != "captured" {
		t.Fatal(initial, repeat)
	}
	count(t, s.aq, "select count(*) from sim_charge", 1)
	change := terminal
	change.Amount++
	call(t, "POST", ac+"/sim/charges", serviceKey, change, 409, nil)

	web := chargeRequest{Operation: "online-attempt", Merchant: merchant, Order: "MIMI-GUIDED-ONLINE", Channel: "online", Amount: 6800, Currency: currency, Brand: "mastercard"}
	var online charge
	call(t, "POST", ac+"/sim/charges", serviceKey, web, 200, &online)
	if online.Status != "awaiting_customer" || !strings.HasPrefix(online.CheckoutURL, ac+"/checkout/") {
		t.Fatal(online)
	}
	view, err := http.Get(online.CheckoutURL)
	if err != nil {
		t.Fatal(err)
	}
	policy := view.Header.Get("Referrer-Policy")
	before, err := io.ReadAll(view.Body)
	view.Body.Close()
	if err != nil || policy != "same-origin" {
		t.Fatalf("hosted checkout form policy %q (%v)", policy, err)
	}
	for _, origin := range []string{"null", "https://unrelated.example"} {
		req, err := http.NewRequest(http.MethodPost, online.CheckoutURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", origin)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 403 {
			t.Fatalf("unexpected checkout from %q: %d", origin, res.StatusCode)
		}
	}
	if !strings.Contains(string(before), "Approve simulated payment") {
		t.Fatal("hosted approval missing")
	}
	call(t, "POST", ac+"/sim/approve/"+web.Operation, serviceKey, nil, 401, nil)
	after := call(t, "POST", online.CheckoutURL, "", nil, 200, nil)
	if !strings.Contains(string(after), "Payment approved") {
		t.Fatal(string(after))
	}
	call(t, "POST", online.CheckoutURL, "", nil, 200, nil)
	call(t, "GET", ac+"/sim/charges/"+web.Operation, serviceKey, nil, 200, &online)
	if online.Status != "captured" {
		t.Fatal(online)
	}
	ref := refundRequest{Operation: "refund-1", Charge: online.Reference, Amount: 1000, Merchant: merchant, Currency: currency}
	var credit, replayed refund
	call(t, "POST", ac+"/sim/refunds", serviceKey, ref, 200, &credit)
	call(t, "POST", ac+"/sim/refunds", serviceKey, ref, 200, &replayed)
	if credit != replayed || credit.Reference == "" || credit.Amount != 1000 || credit.Status != "succeeded" {
		t.Fatal(credit, replayed)
	}
	ref.Operation = "refund-too-large"
	ref.Amount = 6000
	call(t, "POST", ac+"/sim/refunds", serviceKey, ref, 409, nil)
	practice := terminal
	practice.Order = "another-order"
	practice.Operation = "another-op"
	practice.Amount = 1234
	practice.Brand = "amex"
	practice.LoseResponse = false
	call(t, "POST", ac+"/sim/charges", serviceKey, practice, 200, nil)

	req := payoutRequest{Operation: "payout-original", Merchant: merchant, Currency: currency, Batch: "guided"}
	s.dropBankReply.Store(true)
	call(t, "POST", ac+"/sim/payouts", serviceKey, req, 503, nil)
	count(t, s.aq, "select count(*) from sim_payout", 1)
	count(t, s.bk, "select count(*) from sim_bank_entry where posted_at is null", 1)
	var p payout
	call(t, "POST", ac+"/sim/payouts", serviceKey, req, 200, &p)
	if p.Net != 9746 || len(p.Lines) != 5 {
		t.Fatal(p)
	}
	sum := int64(0)
	for _, line := range p.Lines {
		sum += line.Amount
	}
	if sum != p.Net {
		t.Fatal("statement does not reconcile")
	}
	call(t, "GET", bank+"/bank/entries/"+p.Trace, serviceKey, nil, 404, nil)
	call(t, "POST", bank+"/bank/post/"+p.Trace, serviceKey, nil, 401, nil)
	call(t, "POST", bank+"/bank/instructions", serviceKey, bankInstruction{p.Trace, merchant, currency, 1}, 401, nil)
	call(t, "POST", bank+"/bank/instructions", instructKey, bankInstruction{p.Trace, merchant, currency, 1}, 409, nil)
	call(t, "POST", bank+"/bank/post/"+p.Trace, postKey, nil, 200, nil)
	call(t, "POST", bank+"/bank/post/"+p.Trace, postKey, nil, 200, nil)
	var entry bankEntry
	call(t, "GET", bank+"/bank/entries/"+p.Trace, serviceKey, nil, 200, &entry)
	if entry.Status != "posted" || entry.Amount != 9746 || entry.Trace != p.Trace || entry.Merchant != merchant {
		t.Fatal(entry)
	}
	var again payout
	call(t, "POST", ac+"/sim/payouts", serviceKey, req, 200, &again)
	if again.ID != p.ID || again.Trace != p.Trace {
		t.Fatal(again, p)
	}
	req.Batch = "standard"
	call(t, "POST", ac+"/sim/payouts", serviceKey, req, 409, nil)
	req.Operation = "practice-payout"
	call(t, "POST", ac+"/sim/payouts", serviceKey, req, 200, &p)
	if p.Net != 1209 || len(p.Lines) != 2 {
		t.Fatal("practice batch", p)
	}
	req.Operation = "next"
	req.Batch = "guided"
	call(t, "POST", ac+"/sim/payouts", serviceKey, req, 409, nil)
}
func TestCardBrandChannelsAndConcurrentRefund(t *testing.T) {
	s := newTestStore(t)
	ac := s.acquirer.URL
	var last charge
	for _, brand := range []string{"visa", "mastercard", "amex"} {
		for _, channel := range []string{"terminal", "online"} {
			id := brand + "-" + channel
			r := chargeRequest{Operation: id, Order: id, Merchant: merchant, Currency: currency, Amount: 100, Channel: channel, Brand: brand}
			if channel == "terminal" {
				r.Terminal = "MIMI-01"
			}
			call(t, "POST", ac+"/sim/charges", serviceKey, r, 200, &last)
			if channel == "online" {
				call(t, "POST", ac+"/sim/approve/"+id, approveKey, nil, 200, &last)
			}
			if last.Status != "captured" || last.Brand != brand || last.Channel != channel {
				t.Fatal(last)
			}
		}
	}
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for n := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _, _ := request("POST", ac+"/sim/refunds", serviceKey, refundRequest{Operation: fmt.Sprintf("partial-%d", n), Charge: last.Reference, Amount: 80, Merchant: merchant, Currency: currency})
			codes <- status
		}()
	}
	wg.Wait()
	close(codes)
	results := map[int]int{}
	for code := range codes {
		results[code]++
	}
	if results[200] != 1 || results[409] != 1 {
		t.Fatal(results)
	}
	count(t, s.aq, "select count(*) from sim_refund", 1)
}

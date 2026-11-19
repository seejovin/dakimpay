package pay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Each run receives a new database. The supplied connection is solely the
// administrative connection used to create and later drop the fixture.
func isolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DAKIM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set DAKIM_TEST_DATABASE_URL to run PostgreSQL-backed service acceptance")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := newID("dakim_test")
	q := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+q); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+q); err != nil {
			t.Error(err)
		}
	})
	cfg := admin.Config().Copy()
	cfg.ConnConfig.Database = name
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

type obj = map[string]any

func issue(handler http.Handler, method, path, key string, body any) (int, obj) {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	var o obj
	_ = json.Unmarshal(rec.Body.Bytes(), &o)
	return rec.Code, o
}
func TestDurableRecoveryRefundAndIndependentBankProof(t *testing.T) {
	db := isolatedPool(t)
	ctx := context.Background()
	var mu sync.Mutex
	var charges = map[string]obj{}
	var refunds = map[string]obj{}
	var wrongEvidence, bankState, statementFault atomic.Int32
	acquirer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-service-credential" {
			w.WriteHeader(403)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.Method == "GET" {
			f := charges[r.URL.Path[len("/sim/charges/"):]]
			if f == nil {
				w.WriteHeader(404)
				return
			}
			copy := obj{}
			for k, v := range f {
				copy[k] = v
			}
			if wrongEvidence.Load() == 1 {
				copy["amount"] = 1
			}
			answer(w, 200, copy)
			return
		}
		var in obj
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		id := in["operation_id"].(string)
		switch r.URL.Path {
		case "/sim/charges":
			if old := charges[id]; old != nil {
				answer(w, 200, old)
				return
			}
			in["reference"] = "CH-" + id
			in["status"] = "captured"
			charges[id] = in
			if in["lose_response"] == true {
				w.WriteHeader(504)
				return
			}
			answer(w, 200, in)
		case "/sim/refunds":
			if old := refunds[id]; old != nil {
				answer(w, 200, old)
				return
			}
			in["reference"] = "RF-" + id
			in["status"] = "succeeded"
			refunds[id] = in
			answer(w, 200, in)
		case "/sim/payouts":
			var lines []Line
			var net int64
			for _, ch := range charges {
				amt := int64(ch["amount"].(float64))
				ref := ch["reference"].(string)
				lines = append(lines, Line{"L-" + ref, "capture", ref, amt})
				if statementFault.Load() != 1 {
					fee := -(amt*200 + 5000) / 10000
					if statementFault.Load() == 2 {
						fee -= 1
					}
					lines = append(lines, Line{"F-" + ref, "fee", ref, fee})
					net += fee
				}
				net += amt
			}
			for _, rf := range refunds {
				if statementFault.Load() == 3 {
					continue
				}

				amt := -int64(rf["amount"].(float64))
				ref := rf["reference"].(string)
				lines = append(lines, Line{"L-" + ref, "refund", ref, amt})
				net += amt
			}
			answer(w, 200, Payout{ID: "P1", BankTrace: "B1", Currency: "USD", MerchantID: "mimi", Net: net, Lines: lines})
		default:
			w.WriteHeader(404)
		}
	}))
	defer acquirer.Close()
	bank := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bank/entries/B1" || r.Header.Get("Authorization") != "Bearer test-service-credential" {
			w.WriteHeader(403)
			return
		}
		switch bankState.Load() {
		case 0:
			w.WriteHeader(404)
		case 1:
			answer(w, 200, bankEntry{"B1", "mimi", "USD", 1, "posted"})
		case 2:
			answer(w, 200, bankEntry{"B1", "mimi", "USD", 3174, "posted"})
		}
	}))
	defer bank.Close()
	service, err := New(ctx, db, acquirer.URL, bank.URL, "test-service-credential")
	if err != nil {
		t.Fatal(err)
	}
	h := service.Handler()
	req := func(method, path, key string, in any, want int) obj {
		t.Helper()
		code, out := issue(h, method, path, key, in)
		if code != want {
			t.Fatalf("%s %s: got %d want %d: %v", method, path, code, want, out)
		}
		return out
	}
	in := PaymentInput{"ORDER-T1", "terminal", 4200, "USD", "visa", "MIMI-01", true}
	lost := req("POST", "/v1/payments", "sale-1", in, 202)
	pid := lost["id"].(string)
	if lost["status"] != "unknown" {
		t.Fatal(lost)
	}
	req("POST", "/v1/payments", "another-key", in, 409)
	changed := in
	changed.Amount = 1
	req("POST", "/v1/payments", "sale-1", changed, 409)
	// Simulate restart by creating a fresh handler on the original database.
	service, err = New(ctx, db, acquirer.URL, bank.URL, "test-service-credential")
	if err != nil {
		t.Fatal(err)
	}
	h = service.Handler()
	wrongEvidence.Store(1)
	req("POST", "/v1/payments/"+pid+"/recover", "", nil, 502)
	wrongEvidence.Store(0)
	f := req("POST", "/v1/payments/"+pid+"/recover", "", nil, 200)
	if f["status"] != "captured" {
		t.Fatal(f)
	}
	if p := req("POST", "/v1/payments", "sale-1", in, 200); p["id"] != pid {
		t.Fatal(p)
	}
	// An attempt persisted before dispatch must use the original operation on recovery.
	_, err = db.Exec(ctx, `INSERT INTO pay.payments(id,merchant_id,request_key,order_id,channel,amount,currency,brand,terminal_id,lose_response)
 VALUES ('dp_disabled','mimi','sale-2','ORDER-T2','terminal',2100,'USD','amex','MIMI-01',false)`)
	if err != nil {
		t.Fatal(err)
	}
	req("POST", "/v1/payments/dp_disabled/recover", "", nil, 200)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _ := issue(h, "POST", "/v1/payments/"+pid+"/refunds", fmt.Sprintf("refund-%d", i), obj{"amount": 3000})
			codes <- code
		}(i)
	}
	wg.Wait()
	close(codes)
	seen := map[int]int{}
	for c := range codes {
		seen[c]++
	}
	if seen[201] != 1 || seen[409] != 1 {
		t.Fatal("concurrent refund permits", seen)
	}
	d := req("GET", "/v1/dashboard", "", nil, 200)
	refund := d["refunds"].([]any)[0].(map[string]any)
	req("GET", "/v1/refunds/"+refund["id"].(string), "", nil, 200)
	req("POST", "/v1/refunds/"+refund["id"].(string)+"/recover", "", nil, 200)
	for _, f := range []int32{1, 2, 3} {
		statementFault.Store(f)
		req("POST", "/v1/settle", "lost-first-key", nil, 409)
	}
	statementFault.Store(0)
	p := req("POST", "/v1/settle", "settle", nil, 201)
	req("POST", "/v1/settle", "lost-first-key", nil, 200)
	req("GET", "/v1/payouts/P1/statement", "", nil, 409)
	if p["status"] != "in_transit" || p["net"] != float64(3174) {
		t.Fatal(p)
	}
	req("POST", "/v1/settle", "settle", obj{"batch": "guided"}, 409)
	for _, step := range []struct {
		state int32
		code  int
	}{{0, 200}, {1, 409}, {2, 200}, {2, 200}} {
		bankState.Store(step.state)
		req("POST", "/v1/reconcile", "", nil, step.code)
	}
	d = req("GET", "/v1/dashboard", "", nil, 200)
	statement := req("GET", "/v1/payouts/P1/statement", "", nil, 200)
	if statement["depositMinor"] != float64(3174) || len(statement["lines"].([]any)) != 3 {
		t.Fatal(statement)
	}
	byRef := map[string]map[string]any{}
	for _, v := range statement["lines"].([]any) {
		l := v.(map[string]any)
		byRef[l["reference"].(string)] = l
	}
	if byRef[pid]["feeMinor"] != float64(84) || byRef[pid]["grossMinor"] != float64(4200) || byRef[refund["id"].(string)]["grossMinor"] != float64(-3000) {
		t.Fatal(statement)
	}
	totals := d["totals"].(map[string]any)
	if totals["captured"] != float64(6300) || totals["refunded"] != float64(3000) || totals["deposited"] != float64(3174) || totals["pending_deposit"] != float64(0) {
		t.Fatal(totals)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(charges) != 2 || len(refunds) != 1 {
		t.Fatal("duplicate original operation")
	}
}

package virtualrail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"dakimpay/internal/railcontract"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	testConnector = "virtual-test-connector-only"
	testReader    = "virtual-test-reader-one-only"
	secondReader  = "virtual-test-reader-two-only"
)

func fixtureDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DAKIM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("DAKIM_TEST_DATABASE_URL required for virtual processor acceptance")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	nonce, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	name := "vr_" + nonce
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+ident); err != nil {
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

func TestReaderAndHostedProcessor(t *testing.T) {
	ctx := context.Background()
	db := fixtureDB(t)
	programs := []Program{
		{Merchant: "mimi", Account: "acct-1", Beneficiary: "bank-mimi", StatementToken: "mimi-export-credential-test", Currency: "USD", MaxAmount: 100_000, Outlets: []string{"store"}, Terminals: []Terminal{{"reader-1", "store", testReader}}},
		{Merchant: "other", Account: "acct-2", Beneficiary: "bank-other", StatementToken: "other-export-credential-test", Currency: "USD", MaxAmount: 100_000, Outlets: []string{"store"}, Terminals: []Terminal{{"reader-2", "store", secondReader}}},
	}
	start := func() (*httptest.Server, *Client) {
		srv := httptest.NewUnstartedServer(nil)
		origin := "http://" + srv.Listener.Addr().String()
		handler, err := New(ctx, db, origin, testConnector, programs)
		if err != nil {
			t.Fatal(err)
		}
		srv.Config.Handler = handler
		srv.Start()
		t.Cleanup(srv.Close)
		c, err := NewClient(origin, testConnector)
		if err != nil {
			t.Fatal(err)
		}
		return srv, c
	}
	srv, c := start()
	ledger, err := c.Identity(ctx, railcontract.Command{Merchant: "mimi", Account: "acct-1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx = railcontract.WithProcessorIdentity(ctx, ledger)
	wire := func(method, path, token, body, typ string) (int, []byte, error) {
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Origin", srv.URL)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", typ)
		res, err := srv.Client().Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		return res.StatusCode, data, err
	}
	check := func(method, path, token, body, typ string, expected int) []byte {
		t.Helper()
		code, raw, err := wire(method, path, token, body, typ)
		if err != nil || code != expected {
			t.Fatalf("%s %s -> %d %s (%v), want %d", method, path, code, raw, err, expected)
		}
		return raw
	}
	observe := func(cmd railcontract.Command, want string) railcontract.Fact {
		t.Helper()
		f, found, err := c.Lookup(ctx, cmd)
		if err != nil || !found || f.State != want {
			t.Fatalf("operation %s: %+v %t %v", cmd.ID, f, found, err)
		}
		return f
	}
	charge := railcontract.Command{ID: "op-1", Merchant: "mimi", Account: "acct-1", Order: "basket-1", Outlet: "store", Terminal: "reader-1", Channel: "terminal", Kind: "charge", Currency: "USD", Amount: 1234}
	if _, f, err := c.Lookup(ctx, charge); f || err != nil {
		t.Fatalf("absence: %t %v", f, err)
	}
	if err := c.Submit(ctx, charge); err != nil {
		t.Fatal(err)
	}
	if err := c.Submit(ctx, charge); err != nil {
		t.Fatal(err)
	}
	wrong := charge
	wrong.Amount++
	if c.Submit(ctx, wrong) == nil {
		t.Fatal("amount rebound on replay")
	}

	check("GET", "/device/v1/command", testConnector, "", "", 401)
	check("POST", "/device/v1/commands/op-1/decision", secondReader, `{"decision":"approve"}`, "application/json", 404)
	projection := check("GET", "/device/v1/command", testReader, "", "", 200)
	var d deviceCommand
	if err := json.Unmarshal(projection, &d); err != nil || d.Version != 1 || d.Command == nil || d.Command.Command != charge {
		t.Fatalf("reader %s", projection)
	}
	busy := charge
	busy.ID = "op-2"
	busy.Order = "basket-2"
	if err := c.Submit(ctx, busy); err != nil {
		t.Fatal(err)
	}
	observe(busy, "declined")
	other := charge
	other.Account = "acct-2"
	other.Merchant = "other"
	other.Terminal = "reader-2"
	if err := c.Submit(ctx, other); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for i := 0; i < 10; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			code, _, err := wire("POST", "/device/v1/commands/op-1/decision", testReader, `{"decision":"approve"}`, "application/json")
			if code != 200 || err != nil {
				t.Errorf("approval: %d %v", code, err)
			}
		}()
	}
	wait.Wait()
	captured := observe(charge, "captured")
	observe(other, "pending")
	check("POST", "/device/v1/commands/op-1/decision", testReader, `{"decision":"decline"}`, "application/json", 409)
	check("POST", "/device/v1/commands/op-1/decision", secondReader, `{"decision":"decline"}`, "application/json", 200)
	observe(other, "declined")

	// A full replay after the service closes reuses the financial source record.
	srv.Close()
	srv, c = start()
	if err := c.Submit(ctx, charge); err != nil {
		t.Fatal(err)
	}
	if f := observe(charge, "captured"); f.Reference != captured.Reference {
		t.Fatalf("movement changed: %+v", f)
	}
	refund := charge
	refund.ID = "refund"
	refund.Kind = "refund"
	refund.Parent = charge.ID
	refund.Amount = 900
	for i := 0; i < 4; i++ {
		r := refund
		r.ID = fmt.Sprintf("refund-%d", i)
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := c.Submit(ctx, r); err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
	inv, err := c.InspectRelease(ctx, charge)
	if err != nil || !inv.Complete || inv.ObservedAt.IsZero() || inv.Fact.State != "captured" || inv.DisputeStatus != "none" || len(inv.Refunds) != 4 {
		t.Fatalf("original release inventory incomplete: %+v, %v", inv, err)
	}
	var sourceRefunded int64
	for _, movement := range inv.Refunds {
		if movement.State == "refunded" {
			sourceRefunded += movement.Amount
		}
	}
	if sourceRefunded != 900 {
		t.Fatalf("original refund inventory %d, want 900", sourceRefunded)
	}
	var n, sum int
	if err := db.QueryRow(ctx, "SELECT count(*),sum(amount) FROM virtualrail.movement WHERE merchant='mimi'").Scan(&n, &sum); err != nil || n != 2 || sum != 334 {
		t.Fatalf("sources %d/%d: %v", n, sum, err)
	}

	hosted := charge
	hosted.ID = "shop-1"
	hosted.Order = "order-shop-1"
	hosted.Channel = "hosted"
	hosted.Terminal = ""
	if err := c.Submit(ctx, hosted); err != nil {
		t.Fatal(err)
	}
	f := observe(hosted, "pending")
	path := strings.TrimPrefix(f.CheckoutURL, srv.URL)
	if !bytes.Contains(check("GET", path, "", "", "", 200), []byte("USD 12.34")) {
		t.Fatal("buyer amount")
	}
	for i := 0; i < 2; i++ {
		check("POST", path, "", "decision=approve", "application/x-www-form-urlencoded", 200)
	}
	observe(hosted, "captured")
	hosted.ID = "shop-late"
	hosted.Order = "late"
	if err := c.Submit(ctx, hosted); err != nil {
		t.Fatal(err)
	}
	path = strings.TrimPrefix(observe(hosted, "pending").CheckoutURL, srv.URL)
	if _, err := db.Exec(ctx, "UPDATE virtualrail.operation SET deadline=now()-interval '1 second' WHERE id=$1", hosted.ID); err != nil {
		t.Fatal(err)
	}
	check("POST", path, "", "decision=approve", "application/x-www-form-urlencoded", 409)
	observe(hosted, "declined")
	// Two concurrent payout cuts compete for the same original financial
	// movements. Only one cut may freeze the source and no other purpose
	// credential can retrieve or create the original merchant's report.
	const exportKey = "mimi-export-credential-test"
	type cut struct {
		id   string
		code int
		body []byte
		err  error
	}
	cuts := make(chan cut, 2)
	for _, id := range []string{"batch-one", "batch-two"} {
		go func() {
			code, body, err := wire("POST", "/rail/v1/remittances/"+id, exportKey, "", "")
			cuts <- cut{id, code, body, err}
		}()
	}
	var winning cut
	empty := 0
	for range 2 {
		result := <-cuts
		if result.err != nil {
			t.Fatal(result.err)
		}
		switch result.code {
		case 201:
			winning = result
		case 409:
			empty++
		default:
			t.Fatalf("cut: %d %s", result.code, result.body)
		}
	}
	if winning.id == "" || empty != 1 {
		t.Fatal("competing batches duplicated a source")
	}
	var report settlementReport
	if err := json.Unmarshal(winning.body, &report); err != nil || report.Net != 1512 || len(report.Lines) != 5 {
		t.Fatalf("wrong independent settlement: %s, %v", winning.body, err)
	}
	route := "/rail/v1/remittances/" + winning.id
	check("GET", route, testConnector, "", "", 401)
	check("GET", route, testReader, "", "", 401)
	check("GET", route, "other-export-credential-test", "", "", 404)
	raw := check("GET", route+"/source", exportKey, "", "", 200)
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != report.Source {
		t.Fatal("source digest does not bind native artifact")
	}
	if !bytes.Equal(check("POST", route, exportKey, "", "", 200), winning.body) {
		t.Fatal("frozen statement changed")
	}
	additional := charge
	additional.ID = "late-pending"
	additional.Order = "extra"
	if err := c.Submit(ctx, additional); err != nil {
		t.Fatal(err)
	}
	check("POST", "/device/v1/commands/late-pending/decision", testReader, `{"decision":"approve"}`, "application/json", 200)
	srv.Close()
	srv, c = start()
	if !bytes.Equal(check("GET", route, exportKey, "", "", 200), winning.body) {
		t.Fatal("restart lost immutable statement")
	}
	var late settlementReport
	if err := json.Unmarshal(check("POST", "/rail/v1/remittances/late", exportKey, "", "", 201), &late); err != nil || late.Net != 1209 || len(late.Lines) != 2 {
		t.Fatalf("reallocated first statement: %+v %v", late, err)
	}
	// A changed enrollment must be deliberately decommissioned/reprovisioned.
	programs[0].Terminals[0].Token = "different-test-device-credential"
	if _, err := New(ctx, db, srv.URL, testConnector, programs); err == nil {
		t.Fatal("reader silently rebound")
	}
}

func TestOriginalLookupNeedsBoundAbsence(t *testing.T) {
	cmd := railcontract.Command{ID: "a", Merchant: "mimi", Account: "one", Kind: "charge"}
	for _, bad := range []string{`{"error":"not found"}`, `{"code":"operation_absent","merchant_id":"mimi","processor_account":"two","operation_id":"a"}`, `{"code":"unexpected","code":"operation_absent","merchant_id":"mimi","processor_account":"one","operation_id":"a"}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(ledgerHeader, strings.Repeat("a", 64))
			w.WriteHeader(404)
			_, _ = io.WriteString(w, bad)
		}))
		c, err := NewClient(srv.URL, testConnector)
		if err != nil {
			t.Fatal(err)
		}
		_, found, err := c.Lookup(railcontract.WithProcessorIdentity(context.Background(), strings.Repeat("a", 64)), cmd)
		srv.Close()
		if found || err == nil {
			t.Fatalf("accepted ambiguous absence %s", bad)
		}
	}
}

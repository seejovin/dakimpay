package virtualrail

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"dakimpay/internal/railcontract"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLedgerContinuityAtOriginalAddress(t *testing.T) {
	ctx := t.Context()
	original, replacement := fixtureDB(t), fixtureDB(t)
	srv := httptest.NewUnstartedServer(nil)
	origin := "http://" + srv.Listener.Addr().String()
	programs := []Program{{Merchant: "shop", Account: "acquirer", Currency: "USD", MaxAmount: 10000, Outlets: []string{"store"}}}
	var active atomic.Pointer[http.Handler]
	configure := func(db *pgxpool.Pool, token string) *Client {
		t.Helper()
		h, err := New(ctx, db, origin, token, programs)
		if err != nil {
			t.Fatal(err)
		}
		active.Store(&h)
		c, err := NewClient(origin, token)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	client := configure(original, testConnector)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*active.Load()).ServeHTTP(w, r)
	})
	srv.Start()
	t.Cleanup(srv.Close)

	cmd := railcontract.Command{ID: "lost-reply", Order: "basket", Kind: "charge", Merchant: "shop", Account: "acquirer", Currency: "USD", Amount: 1299, Outlet: "store", Channel: "hosted"}
	id, err := client.Identity(ctx, cmd)
	if err != nil || !ledgerPattern.MatchString(id) {
		t.Fatalf("original enrollment: %q %v", id, err)
	}
	pin := railcontract.WithProcessorIdentity(ctx, id)
	if err := client.Submit(pin, cmd); err != nil {
		t.Fatal(err)
	}
	presentation, found, err := client.Lookup(pin, cmd)
	if err != nil || !found || presentation.State != "pending" {
		t.Fatalf("original presentation unavailable: found=%v err=%v", found, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, presentation.CheckoutURL, strings.NewReader("decision=approve"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("buyer decision on original ledger: %d", res.StatusCode)
	}
	rotatedToken := "independent-connector-rotation"
	client = configure(original, rotatedToken)
	if observed, err := client.Identity(pin, cmd); err != nil || observed != id {
		t.Fatalf("credential rotation changed ledger identity: %q %v", observed, err)
	}
	capture, found, err := client.Lookup(pin, cmd)
	if err != nil || !found || capture.State != "captured" || capture.Reference == "" {
		t.Fatalf("original charge lost on restart: found=%v, state=%q, err=%v", found, capture.State, err)
	}
	raw, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/rail/v1/accounts/acquirer/merchants/shop/operations/lost-reply", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw.Header.Set("Authorization", "Bearer "+rotatedToken)
	res, err = srv.Client().Do(raw)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("source inquiry without the ledger pin: %d", res.StatusCode)
	}

	client = configure(replacement, rotatedToken)
	different, err := client.Identity(ctx, cmd)
	if err != nil || different == id {
		t.Fatalf("replacement enrollment: %q %v", different, err)
	}
	expectMismatch := func(err error) {
		t.Helper()
		if !errors.Is(err, railcontract.ErrProcessorIdentityMismatch) {
			t.Fatalf("expected a ledger continuity failure, got %v", err)
		}
	}
	_, err = client.Identity(pin, cmd)
	expectMismatch(err)
	_, _, err = client.Lookup(pin, cmd)
	expectMismatch(err)
	expectMismatch(client.Submit(pin, cmd))
	_, err = client.InspectRelease(pin, cmd)
	expectMismatch(err)
	replacementPin := railcontract.WithProcessorIdentity(ctx, different)
	if _, found, err := client.Lookup(replacementPin, cmd); err != nil || found {
		t.Fatalf("replacement ledger had an effect: %t %v", found, err)
	}

	// Also fence the transaction when the running process's database connection
	// starts seeing a different ledger, even though its HTTP service still
	// carries its startup identity. No new effect or authoritative absence.
	if _, err := replacement.Exec(ctx, "UPDATE virtualrail.ledger_identity SET ledger_id=$1 WHERE singleton", id); err != nil {
		t.Fatal(err)
	}
	_, err = client.Identity(replacementPin, cmd)
	expectMismatch(err)
	_, _, err = client.Lookup(replacementPin, cmd)
	expectMismatch(err)
	expectMismatch(client.Submit(replacementPin, cmd))
	_, err = client.InspectRelease(replacementPin, cmd)
	expectMismatch(err)
	var count int
	if err := replacement.QueryRow(ctx, "SELECT count(*) FROM virtualrail.operation").Scan(&count); err != nil || count != 0 {
		t.Fatalf("replacement ledger received a command: %d %v", count, err)
	}
}

func TestProcessorEvidenceRequiresReturnedLedger(t *testing.T) {
	id := strings.Repeat("a", 64)
	pin := railcontract.WithProcessorIdentity(t.Context(), id)
	cmd := railcontract.Command{ID: "op", Merchant: "shop", Account: "acct"}
	for _, test := range []struct {
		name    string
		headers []string
	}{
		{"missing", nil}, {"duplicate", []string{id, id}}, {"replacement", []string{strings.Repeat("b", 64)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, value := range test.headers {
					w.Header().Add(ledgerHeader, value)
				}
				writeJSON(w, 404, absence{cmdRoute(cmd), "operation_absent", cmd.Merchant, cmd.Account, cmd.ID})
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(server.URL, testConnector)
			if err != nil {
				t.Fatal(err)
			}
			if _, found, err := client.Lookup(pin, cmd); found || err == nil {
				t.Fatal("accepted original absence without proof of the expected ledger")
			}
		})
	}
}

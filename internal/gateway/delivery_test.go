package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"dakimpay/internal/gwstore"
)

func TestDeliveryBoundaries(t *testing.T) {
	ok := Subscription{ID: "m-hooks", Merchant: "m", Account: "a", URL: "http://127.0.0.1:8000/hook", KeyID: "k1", Secret: strings.Repeat("a", 40)}
	if err := ValidateSubscriptions([]Subscription{ok}); err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"https://example.com:80/hook", "http://localhost:80/hook", "http://127.0.0.1:80/hook?key=x", "http://user@127.0.0.1:80/hook", "http://127.0.0.1:99999/hook"} {
		bad := ok
		bad.URL = addr
		if ValidateSubscriptions([]Subscription{bad}) == nil {
			t.Fatal("accepted destination", addr)
		}
	}
	if ValidateSubscriptions([]Subscription{ok, ok}) == nil {
		t.Fatal("duplicate subscription")
	}
	for _, bad := range []string{`{"event_id":"wrong"}`, `{"event_id":"a","event_id":"a"}`, `{"EVENT_ID":"a"}`, `{"event_id":"a"} {}`, `{"event_id":"a","other":false}`, `{}`} {
		if acknowledges([]byte(bad), "a") {
			t.Fatalf("accepted ambiguous acknowledgment %s", bad)
		}
	}
	if !acknowledges([]byte(" { \"event_id\" : \"a\" }\n"), "a") {
		t.Fatal("valid acknowledgement")
	}
}

func TestDeliveryRetryIsolationAndLeases(t *testing.T) {
	ctx := context.Background()
	store := engineTestStore(t)
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	newService := func() *Service { s := New(store, nil); s.Now = func() time.Time { return now }; return s }
	s := newService()
	p := engineTestPrincipal()
	// Publish through the same atomic feed/outbox path used by the payment engine.
	put := func(merchant, account, id string) {
		t.Helper()
		err := store.Update(ctx, func(tx gwstore.Tx) error {
			op := engineOperation{Status: "captured"}
			op.Command.Merchant, op.Command.Account, op.Command.ID = merchant, account, id
			op.Command.Outlet, op.Command.Kind, op.Command.Amount, op.Command.Currency = "shop", "charge", 1234, "USD"
			return enginePublish(tx, &op, now)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	put(p.Merchant, "foreign-account", "wrong-account")
	put("other-merchant", p.Account, "wrong-merchant")
	put(p.Merchant, p.Account, "my-first")
	put(p.Merchant, p.Account, "my-second")
	var lock sync.Mutex
	var bodies [][]byte
	key := strings.Repeat("test-secret-", 4)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var msg deliveryEnvelope
		if json.Unmarshal(raw, &msg) != nil || msg.Merchant != p.Merchant || msg.Version != 1 {
			t.Error("invalid envelope")
			return
		}
		var event engineEvent
		if json.Unmarshal(msg.Event, &event) != nil || event.Account != p.Account || !strings.HasPrefix(event.Operation, "my-") {
			t.Error("tenant leak in delivery", string(raw))
			return
		}
		ts := r.Header.Get("Dakim-Webhook-Timestamp")
		if _, err := strconv.ParseInt(ts, 10, 64); err != nil {
			t.Error(err)
		}
		if r.Header.Get("Dakim-Webhook-Key-Id") != "key1" || r.Header.Get("Dakim-Webhook-Subscription") != "sub" || r.Header.Get("Dakim-Webhook-Version") != "1" {
			t.Error("key binding or version missing")
		}
		mac := hmac.New(sha256.New, []byte(key))
		fmt.Fprintf(mac, "dakim.webhook.v1\n%s\nkey1\nsub\n", ts)
		mac.Write(raw)
		if !hmac.Equal([]byte(r.Header.Get("Dakim-Webhook-Signature")), []byte("v1="+hex.EncodeToString(mac.Sum(nil)))) {
			t.Error("incorrect signature")
		}
		lock.Lock()
		bodies = append(bodies, raw)
		n := len(bodies)
		lock.Unlock()
		switch n {
		case 1:
			// Receiver has committed the event; its valid acknowledgement is lost.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
		case 2:
			io.WriteString(w, `{"event_id":"another-event"}`)
		default:
			json.NewEncoder(w).Encode(map[string]string{"event_id": msg.ID})
		}
	}))
	defer receiver.Close()
	dest := Subscription{ID: "sub", Merchant: p.Merchant, Account: p.Account, KeyID: "key1", Secret: key, URL: receiver.URL + "/hook"}
	client := newDeliveryClient()
	defer client.CloseIdleConnections()
	deliver := func(service *Service, want bool) {
		t.Helper()
		sent, err := service.deliverOne(ctx, client, dest)
		if err != nil || sent != want {
			t.Fatal("delivery outcome", sent, err)
		}
	}
	count := func() int { lock.Lock(); defer lock.Unlock(); return len(bodies) }
	deliver(s, false)
	status, err := s.DeliveryStatus(ctx, p.Merchant, p.Account, "sub")
	if err != nil || status.Cursor != 1 || status.Pending == "" || status.LastFailure != "transport_error" {
		t.Fatal(status, err)
	}
	first := status.Pending
	deliver(s, false)
	if count() != 1 {
		t.Fatal("retry bypassed backoff")
	}
	if _, err = s.DeliveryStatus(ctx, "other-merchant", p.Account, "sub"); err == nil {
		t.Fatal("foreign delivery status")
	}
	other := dest
	other.Account = "foreign-account"
	if _, err = s.claimDelivery(ctx, other); err == nil {
		t.Fatal("subscription ID rebound to another account")
	}

	s = newService()
	now = now.Add(3 * time.Second)
	deliver(s, false)
	status, err = s.DeliveryStatus(ctx, p.Merchant, p.Account, "sub")
	if err != nil || status.Pending != first || status.LastFailure != "unacknowledged" || status.Cursor != 1 {
		t.Fatal(status, err)
	}
	now = now.Add(4 * time.Second)
	deliver(s, true)
	status, err = s.DeliveryStatus(ctx, p.Merchant, p.Account, "sub")
	if err != nil || status.Cursor != 2 || status.LastAck != first || status.Pending != "" {
		t.Fatal(status, err)
	}
	lock.Lock()
	identical := len(bodies) == 3 && bytes.Equal(bodies[0], bodies[1]) && bytes.Equal(bodies[1], bodies[2])
	lock.Unlock()
	if !identical {
		t.Fatal("original event bytes changed during redelivery")
	}

	// A worker that outlived its lease cannot overwrite its successor's decision.
	old, err := s.claimDelivery(ctx, dest)
	if err != nil || old.Pending == "" {
		t.Fatal(old, err)
	}
	if unavailable, err := s.claimDelivery(ctx, dest); err != nil || unavailable.Pending != "" {
		t.Fatal("concurrent lease", unavailable, err)
	}
	now = now.Add(25 * time.Second)
	fresh, err := newService().claimDelivery(ctx, dest)
	if err != nil || fresh.Pending != old.Pending || fresh.Lease == old.Lease {
		t.Fatal(fresh, err)
	}
	if err = s.finishDelivery(ctx, old, ""); err != nil {
		t.Fatal(err)
	}
	status, err = s.DeliveryStatus(ctx, p.Merchant, p.Account, "sub")
	if err != nil || status.Cursor != 2 || status.Pending != fresh.Pending {
		t.Fatal("stale receipt advanced cursor", status, err)
	}
	if failure := transmitDelivery(ctx, client, dest, fresh, now); failure != "" {
		t.Fatal(failure)
	}
	if err = s.finishDelivery(ctx, fresh, ""); err != nil {
		t.Fatal(err)
	}
	deliver(s, false)
	status, err = s.DeliveryStatus(ctx, p.Merchant, p.Account, "sub")
	if err != nil || status.Cursor != 3 || status.LastAck != fresh.Pending || count() != 4 {
		t.Fatal("final cursor", status, err)
	}
}

func TestDeliveryRejectsRedirectionAndOversizedAcknowledgment(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("followed redirect") }))
	defer destination.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
			return
		}
		io.WriteString(w, strings.Repeat(" ", 2050)+`{"event_id":"event"}`)
	}))
	defer first.Close()
	client := newDeliveryClient()
	defer client.CloseIdleConnections()
	job := deliveryState{DeliveryStatus: DeliveryStatus{Pending: "event"}, Payload: []byte(`{}`)}
	for path, want := range map[string]string{"/redirect": "http_307", "/large": "unacknowledged"} {
		target := Subscription{URL: first.URL + path, Secret: strings.Repeat("a", 40), KeyID: "key", ID: "sub"}
		if got := transmitDelivery(context.Background(), client, target, job, time.Now()); got != want {
			t.Fatalf("%s: got %s, want %s", path, got, want)
		}
	}
}

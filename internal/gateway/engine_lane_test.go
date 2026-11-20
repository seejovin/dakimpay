package gateway

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"dakimpay/internal/railcontract"
)

type blockedProcessor struct {
	firstLookup chan struct{}
	once        sync.Once
	mu          sync.Mutex
	facts       map[string]railcontract.Fact
}

func (r *blockedProcessor) Identity(ctx context.Context, c railcontract.Command) (string, error) {
	ledger := strings.Repeat("a", 64)
	if c.ConnectorID == "wallet" {
		ledger = strings.Repeat("b", 64)
	}
	if pin := railcontract.ExpectedProcessorIdentity(ctx); pin != "" && pin != ledger {
		return "", railcontract.ErrProcessorIdentityMismatch
	}
	return ledger, nil
}
func (r *blockedProcessor) Lookup(ctx context.Context, c railcontract.Command) (railcontract.Fact, bool, error) {
	if c.ConnectorID == "cards" {
		r.once.Do(func() { close(r.firstLookup) })
		<-ctx.Done()
		return railcontract.Fact{}, false, ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.facts[c.ID]
	return f, ok, nil
}
func (r *blockedProcessor) Submit(_ context.Context, c railcontract.Command) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.facts[c.ID] = railcontract.Fact{Command: c, State: "captured", Reference: "wallet-001"}
	return nil
}
func TestBlockedProcessorDoesNotStarveUnrelatedWallet(t *testing.T) {
	p := engineTestPrincipal()
	r := &blockedProcessor{firstLookup: make(chan struct{}), facts: map[string]railcontract.Fact{}}
	s := New(engineTestStore(t), r)
	s.SelectRoute = func(c railcontract.Command) (railcontract.Command, error) {
		id := "cards"
		if c.PaymentMethod == "alipay" {
			id = "wallet"
		}
		return setTestRoute(c, id), nil
	}
	first := testIntent()
	first.PaymentMethod = "visa"
	old, _, err := s.createIntent(t.Context(), p, "first", first)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); s.RunWorker(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-r.firstLookup:
	case <-time.After(5 * time.Second):
		t.Fatal("first connector did not enter original inquiry")
	}
	second := first
	second.Order = "new-order"
	second.Channel = "hosted"
	second.Terminal = ""
	second.PaymentMethod = "alipay"
	fresh, _, err := s.createIntent(t.Context(), p, "new", second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	ticks := time.NewTicker(20 * time.Millisecond)
	defer ticks.Stop()
	for {
		if op := readRouteOp(t, s, p, fresh.Command.ID); op.Status == "captured" && op.Reference == "wallet-001" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("unrelated wallet failed to capture during blocked card processor inquiry")
		case <-ticks.C:
		}
	}
	if got := readRouteOp(t, s, p, old.Command.ID); got.EgressCommitted || got.Status == "captured" {
		t.Fatalf("blocked connector incorrectly advanced: %+v", got)
	}
}

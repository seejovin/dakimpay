package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
)

const engineLease = 30 * time.Second

// RunOnce makes a bounded recovery pass. Moving a work ticket forward in a
// committed transaction leases it; a terminated worker never removes the only
// durable instruction. A later worker's lease fences late responses locally.
func (s *Service) RunOnce(ctx context.Context) error {
	connectors, err := s.activeDispatchLanes(ctx)
	if err != nil {
		return err
	}
	for _, connector := range append([]string{""}, connectors...) {
		if err := s.runLane(ctx, connector); err != nil {
			return err
		}
	}
	return nil
}

// runLane bounds the work done in one scheduling pass. Every claim and egress
// remains fenced and safe to compete with another gateway process.
func (s *Service) runLane(ctx context.Context, connector string) error {
	for i := 0; i < 16; i++ {
		claimed, ok, err := s.claimLane(ctx, connector)
		if err != nil || !ok {
			return err
		}
		if err := s.processWork(ctx, claimed); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) activeDispatchLanes(ctx context.Context) ([]string, error) {
	prefix := engineKey("active-dispatch-lanes") + "/"
	var connectors []string
	err := s.Store.View(ctx, func(tx gwstore.Tx) error {
		connectors = connectors[:0]
		after := ""
		for {
			rows, err := tx.Scan(prefix, after, 128)
			if err != nil {
				return err
			}
			for _, row := range rows {
				id := strings.TrimPrefix(row.Key, prefix)
				if !engineIdentifier.MatchString(id) || string(row.Value) != id {
					return errors.New("invalid dispatch lane")
				}
				connectors = append(connectors, id)
			}
			if len(rows) < 128 {
				return nil
			}
			after = rows[len(rows)-1].Key
		}
	})
	return connectors, err
}

// Each operator-provisioned active connector has independent execution. A
// stalled original inquiry can occupy only its connector's runner. Legacy
// tickets retain their global lane, including after an in-place upgrade.
func (s *Service) RunWorker(ctx context.Context) {
	var mutex sync.Mutex
	running := map[string]bool{}
	var workers sync.WaitGroup
	defer workers.Wait()
	launch := func(connector string) {
		mutex.Lock()
		if running[connector] {
			mutex.Unlock()
			return
		}
		running[connector] = true
		mutex.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { mutex.Lock(); delete(running, connector); mutex.Unlock() }()
			if err := s.runLane(ctx, connector); err != nil && ctx.Err() == nil {
				log.Printf("gateway dispatch %q: %v", connector, err)
			}
		}()
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		launch("")
		connectors, err := s.activeDispatchLanes(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("gateway lane discovery: %v", err)
		}
		for _, id := range connectors {
			launch(id)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) claimWork(ctx context.Context) (engineOperation, bool, error) {
	return s.claimLane(ctx, "")
}
func (s *Service) claimLane(ctx context.Context, connector string) (engineOperation, bool, error) {
	var claimed engineOperation
	var ok bool
	now := s.Now().UTC()
	err := s.Store.Update(ctx, func(tx gwstore.Tx) error {
		ok = false
		rows, err := tx.Scan(engineLanePrefix(connector), "", 1)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			if connector != "" {
				return tx.Delete(engineKey("active-dispatch-lanes", connector))
			}
			return nil
		}
		row := rows[0]
		if row.Key >= engineWorkKey(connector, now.Add(time.Millisecond), "") {
			return nil
		}
		var w engineWork
		if err := json.Unmarshal(row.Value, &w); err != nil {
			return err
		}
		op, err := engineGet(tx, w.Merchant, w.ID)
		if err != nil {
			return err
		}
		if err = tx.Delete(row.Key); err != nil {
			return err
		}
		if op.Ticket != row.Key {
			return nil
		}
		if op.Attempts < 32 {
			op.Attempts++
		}
		if err = engineEnqueue(tx, &op, now.Add(engineLease)); err != nil {
			return err
		}
		if err = engineSave(tx, op); err != nil {
			return err
		}
		claimed = op
		ok = true
		return nil
	})
	return claimed, ok, err
}
func (s *Service) processWork(ctx context.Context, op engineOperation) error {
	if s.Rail == nil {
		return errors.New("gateway has no processor")
	}
	// Each round fits within the lease. Even after a timeout, the remote
	// processor may have seen the attempt. Only its scoped lookup can decide.
	call, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	call, err := s.verifyOperationLedger(call, op)
	var fact railcontract.Fact
	var found bool
	if err == nil {
		fact, found, err = s.Rail.Lookup(call, op.Command)
	}
	if err == nil && !found {
		if op.EgressCommitted {
			return s.finishWork(ctx, op, "review", "previously_dispatched_operation_missing", railcontract.Fact{})
		}
		if op.InquiryOnly || op.Status == "pending" || op.CheckoutURL != "" || op.Reference != "" {
			return s.finishWork(ctx, op, "review", "previously_observed_operation_missing", railcontract.Fact{})
		}
		// Quiesce only new instructions. Observations on an original submitted
		// before revocation can still establish its real outcome.
		if s.CanDispatch != nil && !s.CanDispatch(op.Command) {
			return s.finishWork(ctx, op, op.Status, "program_dispatch_suspended", railcontract.Fact{})
		}
		reason, current, checkErr := s.commitDispatch(call, op)
		if checkErr != nil {
			return checkErr
		}
		if !current {
			return nil
		}
		if reason != "" {
			return s.finishWork(ctx, op, op.Status, reason, railcontract.Fact{})
		}
		if err := call.Err(); err != nil {
			return err
		}
		submitErr := s.Rail.Submit(call, op.Command)
		if errors.Is(submitErr, railcontract.ErrProcessorIdentityMismatch) {
			err = submitErr
		} else {
			fact, found, err = s.Rail.Lookup(call, op.Command)
			if err == nil && !found {
				return s.finishWork(ctx, op, "review", "previously_dispatched_operation_missing", railcontract.Fact{})
			}
		}
	}
	if errors.Is(err, railcontract.ErrProcessorIdentityMismatch) {
		return s.finishWork(ctx, op, "review", "processor_ledger_identity_unverified", railcontract.Fact{})
	}
	if errors.Is(err, railcontract.ErrEvidenceConflict) {
		return s.finishWork(ctx, op, "review", "processor_evidence_conflict", fact)
	}
	if err != nil || !found {
		// Keep evidence that the processor already acknowledged presentment.
		state := "unknown"
		if op.Status == "review" {
			state = "review"
		}
		if op.Status == "pending" {
			state = "pending"
		}
		return s.finishWork(ctx, op, state, "awaiting_processor_evidence", railcontract.Fact{})
	}
	if fact.Command != op.Command || !engineValidFact(fact) || (op.FinancialReference != "" && op.FinancialReference != fact.Reference) {
		return s.finishWork(ctx, op, "review", "processor_evidence_conflict", fact)
	}
	err = s.finishWork(ctx, op, fact.State, brandEvidenceIssue(fact), fact)
	// A conflicting movement identity must roll back the posting before the
	// financial exception is recorded in a fresh transaction.
	var conflict financialError
	if errors.As(err, &conflict) {
		return s.finishWork(ctx, op, "review", "financial_evidence_conflict", fact)
	}
	return err
}

// Persist a one-shot egress permit before the HTTP request. A crash between
// this commit and actual send is deliberately ambiguous. Even an authenticated
// negative lookup on a restored processor ledger cannot reauthorize a debit.
func (s *Service) commitDispatch(ctx context.Context, claimed engineOperation) (reason string, current bool, err error) {
	c := claimed.Command
	now := s.Now().UTC()
	err = s.Store.Update(ctx, func(tx gwstore.Tx) error {
		current = false
		reason = ""
		op, err := engineGet(tx, c.Merchant, c.ID)
		if err != nil {
			return err
		}
		current = op.Ticket == claimed.Ticket && op.Ledger == claimed.Ledger && !op.InquiryOnly && !op.EgressCommitted
		if !current {
			return nil
		}
		if s.CurrentProgram != nil {
			p, ok := s.CurrentProgram(c.Merchant)
			if !ok || p.Merchant != c.Merchant || p.Account != c.Account || p.Currency != c.Currency {
				reason = "program_enrollment_changed"
				return nil
			}
			var enrollment engineEnrollment
			bound, err := gwstore.GetJSON(tx, engineKey("processor-enrollment", p.Merchant, p.Account), &enrollment)
			if err != nil {
				return err
			}
			if !bound || enrollment.Beneficiary != p.Beneficiary || enrollment.Currency != p.Currency || (c.RouteID == "" && enrollment.Ledger != op.Ledger) {
				reason = "program_enrollment_changed"
				return nil
			}
		}
		if c.RouteID != "" {
			var registered engineRouteEnrollment
			p := Principal{Merchant: c.Merchant, Account: c.Account}
			bound, err := gwstore.GetJSON(tx, engineRouteKey(p, c.RouteID), &registered)
			if err != nil {
				return err
			}
			if !bound || registered.Ledger != op.Ledger || registered.ConnectorID != c.ConnectorID || registered.ProviderAccount != c.ProviderAccount {
				reason = "original_processor_route_changed"
				return nil
			}
		}
		if c.Kind == "charge" {
			if c.RouteID != "" {
				state, err := readControl(tx, controlNamespace(c.Merchant, c.Account, c.RouteID))
				if err != nil {
					return err
				}
				if state.Suspended {
					reason = "original_processor_route_suspended"
					return nil
				}
			}
			err = checkSaleAdmission(tx, c.Merchant)
			var blocked engineError
			if errors.As(err, &blocked) && blocked.code == "new_sales_suspended" {
				reason = "merchant_sales_suspended"
				return nil
			}
			if err != nil {
				return err
			}
		} else {
			parent, err := engineGet(tx, c.Merchant, c.Parent)
			if err != nil {
				return err
			}
			if parent.Ledger != op.Ledger || !matchesProcessorRoute(parent.Command, c) || parent.Status != "captured" || parent.Reserved < c.Amount {
				return errors.New("inconsistent original refund authority")
			}
			held, err := releaseFinancialHold(tx, parent.Command, parent.Reference)
			if err != nil {
				return err
			}
			if held {
				reason = "original_financial_exception"
				return nil
			}
		}
		op.EgressCommitted = true
		if err = engineSave(tx, op); err != nil {
			return err
		}
		return gwstore.PutJSON(tx, engineEvidencePrefix(c)+"dispatch", engineEvidence{Type: "dispatch_authorization", Decision: "original_submission_permitted", At: now})
	})
	return
}
func engineValidFact(f railcontract.Fact) bool {
	if f.CardBrand != "" && !engineIdentifier.MatchString(f.CardBrand) {
		return false
	}
	switch f.State {
	case "captured":
		return f.Kind == "charge" && engineIdentifier.MatchString(f.Reference) && f.CheckoutURL == ""
	case "refunded":
		return f.Kind == "refund" && engineIdentifier.MatchString(f.Reference) && f.CheckoutURL == ""
	case "declined":
		return f.Reference == "" && f.CheckoutURL == ""
	case "pending":
		if f.Reference != "" {
			return false
		}
		if f.Channel != "hosted" || f.Kind != "charge" {
			return f.CheckoutURL == ""
		}
		u, err := url.Parse(f.CheckoutURL)
		if err != nil || len(f.CheckoutURL) > 8192 || u.User != nil || u.Hostname() == "" || !strings.HasPrefix(u.Path, "/") {
			return false
		}
		if u.Fragment != "" {
			// Hosted Checkout can carry a buyer capability after '#'. Accept
			// only the configured product's canonical test Checkout origin/path.
			session, ok := strings.CutPrefix(u.Path, "/c/pay/cs_test_")
			return u.Scheme == "https" && u.Host == "checkout.stripe.com" && ok &&
				engineIdentifier.MatchString(session) && u.RawPath == "" && !u.ForceQuery && u.RawQuery == ""
		}
		host := u.Hostname()
		ip := net.ParseIP(host)
		return u.Scheme == "https" || (u.Scheme == "http" && (host == "localhost" || (ip != nil && ip.IsLoopback())))
	}
	return false
}
func (s *Service) finishWork(ctx context.Context, claimed engineOperation, state, reason string, fact railcontract.Fact) error {
	now := s.Now().UTC()
	return s.Store.Update(ctx, func(tx gwstore.Tx) error {
		c := claimed.Command
		op, err := engineGet(tx, c.Merchant, c.ID)
		if err != nil {
			return err
		}
		if op.Ticket != claimed.Ticket {
			return nil
		}
		if err := tx.Delete(op.Ticket); err != nil {
			return err
		}
		op.Ticket = ""
		if fact != (railcontract.Fact{}) {
			if err := engineRememberFact(tx, op.Command, fact, state, reason, now); err != nil {
				return err
			}
		}
		if state == "review" || state == "pending" {
			op.InquiryOnly = true
		}
		if reason == "financial_evidence_conflict" {
			op.FinancialReference = fact.Reference
		}
		if state == "review" {
			fact = railcontract.Fact{}
		}
		final := state == "captured" || state == "refunded" || state == "declined"
		if final {
			if err := engineApplyOutcome(tx, op, state, fact, now); err != nil {
				return err
			}
		} else if state != "review" {
			delay := time.Second
			if reason != "" {
				delay = time.Second * time.Duration(1<<min(op.Attempts-1, 6))
			}
			if err := engineEnqueue(tx, &op, now.Add(delay)); err != nil {
				return err
			}
		}
		link := fact.CheckoutURL
		if state == "pending" && reason != "" {
			link = op.CheckoutURL
		}
		changed := op.CardBrand != fact.CardBrand || op.Status != state || op.Reason != reason || op.Reference != fact.Reference || op.CheckoutURL != link
		op.CardBrand = fact.CardBrand
		op.Status = state
		op.Reference = fact.Reference
		op.CheckoutURL = link
		op.Reason = reason
		if changed {
			return enginePublish(tx, &op, now)
		}
		return engineSave(tx, op)
	})
}
func engineApplyOutcome(tx gwstore.Tx, op engineOperation, state string, fact railcontract.Fact, now time.Time) error {
	c := op.Command
	if state == "captured" || state == "refunded" {
		if err := postConfirmedMovement(tx, c, fact.Reference, fact.CardBrand, now); err != nil {
			return err
		}
	}
	if c.Kind == "refund" {
		parent, err := engineGet(tx, c.Merchant, c.Parent)
		if err != nil {
			return err
		}
		if parent.Status != "captured" || parent.Reserved < c.Amount {
			return fmt.Errorf("inconsistent parent reservation for %s", c.ID)
		}
		parent.Reserved -= c.Amount
		if state == "refunded" {
			parent.Refunded += c.Amount
		}
		return enginePublish(tx, &parent, now)
	}
	var order engineOrder
	key := engineKey("order", c.Merchant, c.Order)
	found, err := gwstore.GetJSON(tx, key, &order)
	if err != nil {
		return err
	}
	if !found || order.Reserved < c.Amount {
		return fmt.Errorf("inconsistent order reservation for %s", c.ID)
	}
	order.Reserved -= c.Amount
	if state == "captured" {
		order.Captured += c.Amount
	}
	if err := gwstore.PutJSON(tx, key, order); err != nil {
		return err
	}
	if c.Channel == "terminal" {
		key = engineKey("reader", c.Merchant, c.Terminal)
		var owner string
		found, err = gwstore.GetJSON(tx, key, &owner)
		if err != nil {
			return err
		}
		if !found || owner != c.ID {
			return fmt.Errorf("inconsistent terminal owner for %s", c.ID)
		}
		return tx.Delete(key)
	}
	return nil
}

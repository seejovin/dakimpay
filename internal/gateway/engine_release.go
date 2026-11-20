package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
)

type engineReleaseDecision struct {
	Payment           engineView `json:"payment"`
	Eligible          bool       `json:"release_eligible"`
	Complete          bool       `json:"complete"`
	Reason            string     `json:"reason"`
	ProcessorAt       *time.Time `json:"processor_observed_at,omitempty"`
	GatewayAt         time.Time  `json:"gateway_observed_at"`
	Reference         string     `json:"processor_capture_reference,omitempty"`
	ProcessorRefunded int64      `json:"processor_refunded_minor"`
	Reserved          int64      `json:"reserved_refund_minor"`
	Dispute           string     `json:"dispute_status"`
	FinanceHold       bool       `json:"financial_exception"`
}

func (s *Service) releaseEvidence(w http.ResponseWriter, r *http.Request, p Principal) error {
	id := r.PathValue("id")
	if !engineIdentifier.MatchString(id) {
		return engineFail(404, "operation_not_found")
	}
	read := func(tx gwstore.Tx) (engineOperation, error) {
		op, err := engineGet(tx, p.Merchant, id)
		if err == nil && (!engineVisible(p, op) || op.Command.Kind != "charge") {
			return engineOperation{}, engineFail(404, "operation_not_found")
		}
		return op, err
	}
	var initial engineOperation
	err := s.Store.View(r.Context(), func(tx gwstore.Tx) error { var err error; initial, err = read(tx); return err })
	if err != nil {
		return err
	}
	result := engineReleaseDecision{Payment: enginePresent(initial), GatewayAt: s.Now().UTC(), Dispute: "unknown", Reserved: initial.Reserved, Reason: "original_not_captured"}
	if initial.Status != "captured" {
		engineSend(w, 200, result)
		return nil
	}
	inspector, ok := s.Rail.(railcontract.ReleaseInspector)
	if !ok {
		return engineFail(503, "processor_inventory_unavailable")
	}
	call, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	call, err = s.verifyOperationLedger(call, initial)
	var inventory railcontract.ReleaseInventory
	if err == nil {
		inventory, err = inspector.InspectRelease(call, initial.Command)
	}
	if errors.Is(err, railcontract.ErrProcessorIdentityMismatch) {
		result.Reason = "processor_ledger_identity_unverified"
		engineSend(w, 200, result)
		return nil
	}
	if errors.Is(err, railcontract.ErrEvidenceConflict) {
		result.Reason = "processor_evidence_conflict"
		engineSend(w, 200, result)
		return nil
	}
	if err != nil {
		return engineFail(503, "processor_inventory_unavailable")
	}

	// Check the gateway ledger and local refund reservations in one fresh
	// read snapshot after the processor enquiry. Expose both observation
	// times; a later external refund cannot be ruled out by any past read.
	err = s.Store.View(r.Context(), func(tx gwstore.Tx) error {
		current, err := read(tx)
		if err != nil {
			return err
		}
		now := s.Now().UTC()
		result = engineReleaseDecision{Payment: enginePresent(current), Reserved: current.Reserved, Dispute: "unknown", GatewayAt: now, Reason: "processor_evidence_conflict"}
		if err := checkEngineEnrollmentTerms(tx, p); err != nil {
			var mismatch engineError
			if !errors.As(err, &mismatch) {
				return err
			}
			result.Reason = mismatch.code
			return nil
		}
		if !inventory.Complete || inventory.Refunds == nil || len(inventory.Refunds) > 256 {
			result.Reason = "processor_inventory_incomplete"
			return nil
		}
		if inventory.ObservedAt.IsZero() || now.Sub(inventory.ObservedAt) > 30*time.Second || inventory.ObservedAt.Sub(now) > 5*time.Second {
			result.Reason = "processor_inventory_stale"
			return nil
		}
		if current.Ledger != initial.Ledger || current.Status != "captured" || inventory.Fact.Command != current.Command || inventory.Fact.State != "captured" || !engineValidFact(inventory.Fact) || inventory.Fact.Reference != current.Reference {
			return nil
		}
		if issue := brandEvidenceIssue(inventory.Fact); issue != "" {
			result.Reason = issue
			return nil
		}
		if inventory.Fact.CardBrand != current.CardBrand {
			return nil
		}
		observed := inventory.ObservedAt.UTC()
		result.ProcessorAt = &observed
		result.Reference = current.Reference
		switch inventory.DisputeStatus {
		case "none", "open", "lost", "won", "pending", "unknown":
			result.Dispute = inventory.DisputeStatus
		default:
			return nil
		}
		ids := make(map[string]bool)
		refs := make(map[string]bool)
		pending := false
		for _, refund := range inventory.Refunds {
			if !matchesProcessorRoute(refund.Command, current.Command) || !engineIdentifier.MatchString(refund.ID) || ids[refund.ID] || refund.Kind != "refund" || refund.Parent != current.Command.ID || refund.Merchant != current.Command.Merchant || refund.Account != current.Command.Account || refund.Order != current.Command.Order || refund.Channel != current.Command.Channel || refund.Outlet != current.Command.Outlet || refund.Terminal != current.Command.Terminal || refund.Currency != current.Command.Currency || refund.Amount <= 0 || refund.Amount > maxMoney || !engineValidFact(refund) {
				return nil
			}
			ids[refund.ID] = true
			switch refund.State {
			case "refunded":
				if refs[refund.Reference] || result.ProcessorRefunded > 1<<63-1-refund.Amount {
					return nil
				}
				refs[refund.Reference] = true
				result.ProcessorRefunded += refund.Amount
			case "pending":
				pending = true
			case "declined":
			default:
				return nil
			}
		}
		result.FinanceHold, err = releaseFinancialHold(tx, current.Command, current.Reference)
		if err != nil {
			return err
		}
		result.Complete = true
		switch {
		case result.FinanceHold:
			result.Reason = "financial_exception"
		case result.Dispute != "none":
			result.Reason = "dispute_not_clear"
		case pending || result.Reserved > 0:
			result.Reason = "refund_pending"
		case result.ProcessorRefunded != current.Refunded:
			result.Reason = "refund_evidence_differs"
		case result.ProcessorRefunded > 0:
			result.Reason = "original_refunded"
		default:
			result.Eligible = true
			result.Reason = "verified_capture"
		}
		return nil
	})
	if err != nil {
		return err
	}
	engineSend(w, 200, result)
	return nil
}

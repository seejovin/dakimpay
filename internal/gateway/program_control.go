package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"dakimpay/internal/gwstore"
)

// SuspensionVersion changes on every distinct freeze, even while already
// suspended. Thus a new incident invalidates every earlier resume proposal.
type salesControl struct {
	Suspended bool  `json:"suspended"`
	Version   int64 `json:"suspension_version"`
	Sequence  int64 `json:"audit_sequence"`
}

type salesProposal struct {
	RouteID  string    `json:"route_id,omitempty"`
	Account  string    `json:"processor_account,omitempty"`
	ID       string    `json:"proposal_id"`
	Merchant string    `json:"merchant_id"`
	Maker    string    `json:"maker"`
	Version  int64     `json:"suspension_version"`
	Reason   string    `json:"reason"`
	Expires  time.Time `json:"expires_at"`
}

type salesAudit struct {
	RouteID  string     `json:"route_id,omitempty"`
	Account  string     `json:"processor_account,omitempty"`
	Sequence int64      `json:"sequence"`
	Action   string     `json:"action"`
	Subject  string     `json:"subject"`
	Role     Role       `json:"role"`
	Merchant string     `json:"merchant_id"`
	Reason   string     `json:"reason,omitempty"`
	Version  int64      `json:"suspension_version"`
	Proposal string     `json:"proposal_id,omitempty"`
	Digest   string     `json:"proposal_digest,omitempty"`
	Expires  *time.Time `json:"expires_at,omitempty"`
	At       time.Time  `json:"recorded_at"`
}

func controlStateKey(merchant string) string { return engineKey("sales-control", merchant, "state") }

func readControl(tx gwstore.Tx, merchant string) (salesControl, error) {
	var state salesControl
	_, err := gwstore.GetJSON(tx, controlStateKey(merchant), &state)
	return state, err
}

func checkSaleAdmission(tx gwstore.Tx, merchant string) error {
	state, err := readControl(tx, merchant)
	if err == nil && state.Suspended {
		return engineFail(409, "new_sales_suspended")
	}
	return err
}

func (s *Service) saleDispatchOpen(ctx context.Context, merchant string) (bool, error) {
	var state salesControl
	err := s.Store.View(ctx, func(tx gwstore.Tx) error {
		var err error
		state, err = readControl(tx, merchant)
		return err
	})
	return !state.Suspended && err == nil, err
}

func controlAuth(roles []Role, h func(http.ResponseWriter, *http.Request, Principal) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok {
			engineSendError(w, engineFail(401, "authentication_required"))
			return
		}
		if !slices.Contains(roles, p.Role) || !engineIdentifier.MatchString(p.Merchant) || !engineIdentifier.MatchString(p.Subject) {
			engineSendError(w, engineFail(403, "program_control_forbidden"))
			return
		}
		if err := h(w, r, p); err != nil {
			engineSendError(w, err)
		}
	}
}

func controlNamespace(merchant, account, route string) string {
	if route == "" {
		return merchant
	}
	return merchant + "/routes/" + account + "/" + route
}

func (s *Service) registerControl(m *http.ServeMux) {
	controlRoles := []Role{FinanceRole, ReviewerRole}
	for _, base := range []string{"/v2/program/sales", "/v2/program/routes/{route}/sales"} {
		register := func(method, path string, roles []Role, h func(http.ResponseWriter, *http.Request, Principal) error) {
			m.HandleFunc(method+" "+base+path, controlAuth(roles, func(w http.ResponseWriter, r *http.Request, p Principal) error {
				if route := r.PathValue("route"); route != "" {
					if !engineIdentifier.MatchString(route) || s.RouteExists == nil || !s.RouteExists(p.Merchant, p.Account, route) {
						return engineFail(404, "route_not_found")
					}
				}
				return h(w, r, p)
			}))
		}
		register("GET", "", []Role{MerchantRole, FinanceRole, ReviewerRole}, s.controlStatus)
		register("GET", "/audit", controlRoles, s.controlAudit)
		register("POST", "/freeze", controlRoles, s.controlFreeze)
		register("POST", "/resume-proposals", []Role{FinanceRole}, s.controlPropose)
		register("POST", "/resume-approvals", []Role{ReviewerRole}, s.controlApprove)
	}
}

func (s *Service) controlStatus(w http.ResponseWriter, r *http.Request, p Principal) error {
	var state salesControl
	err := s.Store.View(r.Context(), func(tx gwstore.Tx) error {
		var err error
		state, err = readControl(tx, controlNamespace(p.Merchant, p.Account, r.PathValue("route")))
		return err
	})
	if err == nil {
		engineSend(w, 200, state)
	}
	return err
}

func (s *Service) controlAudit(w http.ResponseWriter, r *http.Request, p Principal) error {
	var after int64
	if raw := r.URL.Query().Get("after"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return engineFail(400, "invalid_audit_cursor")
		}
		after = n
	}
	prefix := engineKey("sales-control", controlNamespace(p.Merchant, p.Account, r.PathValue("route")), "audit") + "/"
	events := []salesAudit{}
	err := s.Store.View(r.Context(), func(tx gwstore.Tx) error {
		rows, err := tx.Scan(prefix, prefix+fmt.Sprintf("%020d", after), 100)
		if err != nil {
			return err
		}
		for _, row := range rows {
			var event salesAudit
			if err := json.Unmarshal(row.Value, &event); err != nil {
				return err
			}
			events = append(events, event)
		}
		return nil
	})
	if err == nil {
		next := after
		if len(events) > 0 {
			next = events[len(events)-1].Sequence
		}
		engineSend(w, 200, map[string]any{"events": events, "next_after": next})
	}
	return err
}

type controlBinding struct {
	Hash    string     `json:"hash"`
	Receipt salesAudit `json:"receipt"`
}

// An idempotency key is bound to the actor, merchant, operation and request.
// A matching retry returns its original receipt without modifying current state.
func (s *Service) controlWrite(w http.ResponseWriter, r *http.Request, p Principal, action string, input any, apply func(gwstore.Tx, *salesControl, *salesAudit) error) error {
	key := r.Header.Get("Idempotency-Key")
	if !engineIdentifier.MatchString(key) {
		return engineFail(400, "invalid_idempotency_key")
	}
	hash, err := engineHash(input)
	if err != nil {
		return err
	}
	route := r.PathValue("route")
	scope := controlNamespace(p.Merchant, p.Account, route)
	bindingKey := engineKey("sales-control", scope, "key", string(p.Role), p.Subject, action, key)
	at := s.Now().UTC()
	var receipt salesAudit
	err = s.Store.Update(r.Context(), func(tx gwstore.Tx) error {
		var old controlBinding
		found, err := gwstore.GetJSON(tx, bindingKey, &old)
		if err != nil {
			return err
		}
		if found {
			if old.Hash != hash {
				return engineFail(409, "idempotency_conflict")
			}
			receipt = old.Receipt
			return nil
		}
		state, err := readControl(tx, scope)
		if err != nil {
			return err
		}
		if state.Sequence == math.MaxInt64 {
			return fmt.Errorf("program control sequence exhausted")
		}
		state.Sequence++
		receipt = salesAudit{Sequence: state.Sequence, Action: action, Subject: p.Subject, Role: p.Role, Merchant: p.Merchant, At: at}
		if route != "" {
			receipt.Account = p.Account
			receipt.RouteID = route
		}
		if err := apply(tx, &state, &receipt); err != nil {
			return err
		}
		receipt.Version = state.Version
		auditKey := engineKey("sales-control", scope, "audit", fmt.Sprintf("%020d", state.Sequence))
		if _, exists, err := tx.Get(auditKey); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("program audit sequence already recorded")
		}
		if err := gwstore.PutJSON(tx, auditKey, receipt); err != nil {
			return err
		}
		if err := gwstore.PutJSON(tx, controlStateKey(scope), state); err != nil {
			return err
		}
		return gwstore.PutJSON(tx, bindingKey, controlBinding{hash, receipt})
	})
	if err == nil {
		engineSend(w, 200, receipt)
	}
	return err
}

func validControlReason(s string) bool {
	return len(strings.TrimSpace(s)) > 0 && len(s) <= 512 && strings.IndexFunc(s, unicode.IsControl) < 0
}

func (s *Service) controlFreeze(w http.ResponseWriter, r *http.Request, p Principal) error {
	var in struct {
		Reason string `json:"reason"`
	}
	if err := engineDecode(r, &in); err != nil {
		return err
	}
	if !validControlReason(in.Reason) {
		return engineFail(400, "invalid_reason")
	}
	return s.controlWrite(w, r, p, "freeze", in, func(_ gwstore.Tx, state *salesControl, event *salesAudit) error {
		if state.Version == math.MaxInt64 {
			return fmt.Errorf("suspension version exhausted")
		}
		state.Version++
		state.Suspended = true
		event.Reason = in.Reason
		return nil
	})
}

func (s *Service) controlPropose(w http.ResponseWriter, r *http.Request, p Principal) error {
	var in struct {
		Version int64  `json:"suspension_version"`
		Reason  string `json:"reason"`
	}
	if err := engineDecode(r, &in); err != nil {
		return err
	}
	if in.Version < 1 || !validControlReason(in.Reason) {
		return engineFail(400, "invalid_resume_proposal")
	}
	id, err := engineID("resume_")
	if err != nil {
		return err
	}
	return s.controlWrite(w, r, p, "propose_resume", in, func(tx gwstore.Tx, state *salesControl, event *salesAudit) error {
		if !state.Suspended || state.Version != in.Version {
			return engineFail(409, "stale_suspension")
		}
		proposal := salesProposal{ID: id, Merchant: p.Merchant, Maker: p.Subject, Version: state.Version, Reason: in.Reason, Expires: event.At.Add(15 * time.Minute), Account: event.Account, RouteID: event.RouteID}
		digest, err := engineHash(proposal)
		if err != nil {
			return err
		}
		event.Proposal, event.Digest, event.Expires, event.Reason = id, digest, &proposal.Expires, in.Reason
		return gwstore.PutJSON(tx, engineKey("sales-control", controlNamespace(p.Merchant, p.Account, r.PathValue("route")), "proposal", id), proposal)
	})
}

func (s *Service) controlApprove(w http.ResponseWriter, r *http.Request, p Principal) error {
	var in struct {
		ID      string `json:"proposal_id"`
		Digest  string `json:"proposal_digest"`
		Version int64  `json:"suspension_version"`
	}
	if err := engineDecode(r, &in); err != nil {
		return err
	}
	digest, err := hex.DecodeString(in.Digest)
	if err != nil || len(digest) != 32 || in.Version < 1 || !engineIdentifier.MatchString(in.ID) {
		return engineFail(400, "invalid_resume_approval")
	}
	return s.controlWrite(w, r, p, "approve_resume", in, func(tx gwstore.Tx, state *salesControl, event *salesAudit) error {
		var proposal salesProposal
		found, err := gwstore.GetJSON(tx, engineKey("sales-control", controlNamespace(p.Merchant, p.Account, r.PathValue("route")), "proposal", in.ID), &proposal)
		if err != nil {
			return err
		}
		if !found {
			return engineFail(404, "proposal_not_found")
		}
		if proposal.Maker == p.Subject {
			return engineFail(403, "independent_reviewer_required")
		}
		if !state.Suspended || proposal.Version != state.Version || in.Version != state.Version {
			return engineFail(409, "stale_suspension")
		}
		expected, err := engineHash(proposal)
		if err != nil {
			return err
		}
		if expected != in.Digest {
			return engineFail(409, "proposal_digest_mismatch")
		}
		if !s.Now().UTC().Before(proposal.Expires) {
			return engineFail(409, "proposal_expired")
		}
		state.Suspended = false
		event.Proposal, event.Digest, event.Reason = proposal.ID, expected, proposal.Reason
		return nil
	})
}

package gateway

import (
	"context"
	"errors"
	"regexp"
	"time"

	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
)

var engineLedgerID = regexp.MustCompile("^[0-9a-f]{64}$")

type engineEnrollment struct {
	Ledger      string `json:"processor_ledger"`
	Beneficiary string `json:"beneficiary"`
	Currency    string `json:"currency"`
}

type engineIdentityRequest struct {
	command  railcontract.Command
	expected string
}

func (engineIdentityRequest) Error() string { return "processor identity lookup required" }

// GrantFingerprint hashes the canonical credential-derived grant, including its subject and scope.
func GrantFingerprint(p Principal) (string, error) { return engineHash(p) }

type engineAttestation struct {
	Command railcontract.Command
	Ledger  string
}

func (a engineAttestation) forCommand(c railcontract.Command) string {
	if a.Command != c {
		return ""
	}
	return a.Ledger
}
func (s *Service) admitWithIdentity(ctx context.Context, fn func(gwstore.Tx, engineAttestation) error) error {
	var proof engineAttestation
	// An incident can drain a route while its identity inquiry is in flight.
	// Re-read controls at commit and only use attestation of the exact command.
	for i := 0; i < 6; i++ {
		err := s.Store.Update(ctx, func(tx gwstore.Tx) error { return fn(tx, proof) })
		var request engineIdentityRequest
		if !errors.As(err, &request) {
			return err
		}
		provider, ok := s.Rail.(railcontract.ProcessorIdentity)
		if !ok {
			return engineFail(503, "processor_identity_unavailable")
		}
		call, cancel := context.WithTimeout(railcontract.WithProcessorIdentity(ctx, request.expected), 5*time.Second)
		ledger, err := provider.Identity(call, request.command)
		cancel()
		if errors.Is(err, railcontract.ErrProcessorIdentityMismatch) {
			return engineFail(409, "processor_ledger_changed")
		}
		if err != nil || !engineLedgerID.MatchString(ledger) {
			return engineFail(503, "processor_identity_unavailable")
		}
		proof = engineAttestation{Command: request.command, Ledger: ledger}
	}
	return engineFail(503, "route_policy_changing")
}

func checkEngineEnrollmentTerms(tx gwstore.Tx, p Principal) error {
	var e engineEnrollment
	found, err := gwstore.GetJSON(tx, engineKey("processor-enrollment", p.Merchant, p.Account), &e)
	if err != nil {
		return err
	}
	if found && (e.Beneficiary != p.Beneficiary || e.Currency != p.Currency) {
		return engineFail(409, "program_enrollment_changed")
	}
	return nil
}

// Bind the first admitted instruction and the account enrollment atomically.
// A populated account without its original identity requires independent
// migration evidence. Automatic adoption would destroy the original fence.
func engineEnroll(tx gwstore.Tx, p Principal, command railcontract.Command, observed, original string) error {
	key := engineKey("processor-enrollment", p.Merchant, p.Account)
	var enrolled engineEnrollment
	found, err := gwstore.GetJSON(tx, key, &enrolled)
	if err != nil {
		return err
	}
	if found && (enrolled.Beneficiary != p.Beneficiary || enrolled.Currency != p.Currency) {
		return engineFail(409, "program_enrollment_changed")
	}
	if !found {
		rows, err := tx.Scan(engineKey("operations", p.Merchant)+"/", "", 1)
		if err != nil {
			return err
		}
		if original != "" || len(rows) != 0 {
			return engineFail(409, "legacy_processor_enrollment_required")
		}
	}
	if command.RouteID != "" {
		// A legacy unbound ledger already owns its program account's native
		// financial references. Migrating that history into an explicit route
		// requires a verified mapping rather than opening a second namespace.
		if found && observed != "" && enrolled.Ledger == observed && command.ProviderAccount == p.Account {
			return engineFail(409, "legacy_processor_route_migration_required")
		}
		if err := enrollProcessorRoute(tx, p, command, observed, original); err != nil {
			return err
		}
	} else {
		// Historical unbound commands use the original enrollment ledger.
		if found && (!engineLedgerID.MatchString(enrolled.Ledger) || original != "" && original != enrolled.Ledger) {
			return engineFail(409, "processor_ledger_changed")
		}
		if observed == "" {
			return engineIdentityRequest{command, enrolled.Ledger}
		}
		if found && enrolled.Ledger != observed {
			return engineFail(409, "processor_ledger_changed")
		}
	}
	if found {
		return nil
	}
	if err := checkBookBeneficiary(tx, bookPrefix(p.Merchant, p.Account, p.Currency), p.Beneficiary, true); err != nil {
		var conflict financialError
		if errors.As(err, &conflict) {
			return engineFail(409, "program_beneficiary_changed")
		}
		return err
	}
	program := engineEnrollment{Beneficiary: p.Beneficiary, Currency: p.Currency}
	if command.RouteID == "" {
		program.Ledger = observed
	}
	return gwstore.PutJSON(tx, key, program)
}

func (s *Service) verifyOperationLedger(ctx context.Context, op engineOperation) (context.Context, error) {
	if !engineLedgerID.MatchString(op.Ledger) {
		return ctx, railcontract.ErrProcessorIdentityMismatch
	}
	call := railcontract.WithProcessorIdentity(ctx, op.Ledger)
	provider, ok := s.Rail.(railcontract.ProcessorIdentity)
	if !ok {
		return call, errors.New("processor identity unavailable")
	}
	got, err := provider.Identity(call, op.Command)
	if err != nil {
		return call, err
	}
	if got != op.Ledger {
		return call, railcontract.ErrProcessorIdentityMismatch
	}
	return call, nil
}

func engineRecordAdmission(tx gwstore.Tx, op engineOperation, p Principal) error {
	digest, err := GrantFingerprint(p)
	if err != nil {
		return err
	}
	return gwstore.PutJSON(tx, engineEvidencePrefix(op.Command)+"admission", engineEvidence{PaymentMethod: op.Command.PaymentMethod, RouteID: op.Command.RouteID, ConnectorID: op.Command.ConnectorID, ProviderAccount: op.Command.ProviderAccount, Ledger: op.Ledger, Type: "command_admission", Decision: "authorized", Actor: p.Subject, Role: p.Role, GrantDigest: digest, At: op.Created})
}

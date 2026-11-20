package virtualrail

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"

	"dakimpay/internal/railcontract"
	"github.com/jackc/pgx/v5"
)

const ledgerHeader = "Dakim-Processor-Ledger"

var ledgerPattern = regexp.MustCompile("^[a-f0-9]{64}$")

type routeIdentity struct {
	RouteID         string `json:"route_id,omitempty"`
	ConnectorID     string `json:"connector_id,omitempty"`
	ProviderAccount string `json:"provider_account,omitempty"`
}

func cmdRoute(cmd railcontract.Command) routeIdentity {
	return routeIdentity{cmd.RouteID, cmd.ConnectorID, cmd.ProviderAccount}
}

type ledgerIdentity struct {
	routeIdentity
	ID       string `json:"ledger_id"`
	Merchant string `json:"merchant_id"`
	Account  string `json:"processor_account"`
}

// Called under the schema install lock. The identity survives address changes,
// connector credential rotation and restarts using the same processor ledger.
func installLedgerIdentity(ctx context.Context, tx pgx.Tx) (string, error) {
	var id string
	err := tx.QueryRow(ctx, "SELECT ledger_id FROM virtualrail.ledger_identity WHERE singleton").Scan(&id)
	if !errors.Is(err, pgx.ErrNoRows) {
		return id, err
	}
	var entropy [32]byte
	if _, err = rand.Read(entropy[:]); err != nil {
		return "", err
	}
	id = hex.EncodeToString(entropy[:])
	_, err = tx.Exec(ctx, "INSERT INTO virtualrail.ledger_identity(singleton, ledger_id) VALUES (true, $1)", id)
	return id, err
}

func (s *service) requireLedger(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(ledgerHeader, s.ledgerID)
		headers := r.Header.Values(ledgerHeader)
		if len(headers) != 1 || headers[0] != s.ledgerID {
			writeFailure(w, reject(http.StatusPreconditionFailed, "A matching processor ledger identity is required."))
			return
		}
		h(w, r)
	}
}

// A connection pool can acquire a fresh connection after database failover.
// Check the database that actually reads facts or writes the effect, inside
// that same transaction. Concurrent writers may share the identity row lock.
func (s *service) ledgerTx(ctx context.Context, opts pgx.TxOptions, f func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.db, opts, func(tx pgx.Tx) error {
		query := "SELECT ledger_id FROM virtualrail.ledger_identity WHERE singleton"
		if opts.AccessMode != pgx.ReadOnly {
			query += " FOR SHARE"
		}
		var id string
		err := tx.QueryRow(ctx, query).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && id != s.ledgerID) {
			return reject(http.StatusPreconditionFailed, "Processor ledger identity changed.")
		}
		if err != nil {
			return err
		}
		return f(tx)
	})
}

func (s *service) identityHTTP(w http.ResponseWriter, r *http.Request) {
	m, a := r.PathValue("merchant"), r.PathValue("account")
	p, ok := s.programs[a]
	w.Header().Set(ledgerHeader, s.ledgerID)
	if !ok || m != p.Merchant {
		writeFailure(w, reject(http.StatusNotFound, "Processor program not found."))
		return
	}
	if expected := r.Header.Values(ledgerHeader); len(expected) > 0 && (len(expected) != 1 || expected[0] != s.ledgerID) {
		writeFailure(w, reject(http.StatusPreconditionFailed, "Processor ledger identity changed."))
		return
	}
	err := s.ledgerTx(r.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var enrolled bool
		if err := tx.QueryRow(r.Context(), "SELECT EXISTS (SELECT 1 FROM virtualrail.program WHERE merchant=$1 AND account=$2 AND currency=$3)", m, a, p.Currency).Scan(&enrolled); err != nil {
			return err
		}
		if !enrolled {
			return reject(http.StatusPreconditionFailed, "Processor program enrollment changed.")
		}
		return nil
	})
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ledgerIdentity{routeIdentity{p.RouteID, p.ConnectorID, p.ProviderAccount}, s.ledgerID, m, a})
}

func (c *Client) Identity(ctx context.Context, cmd railcontract.Command) (string, error) {
	path := "/rail/v1/accounts/" + url.PathEscape(cmd.Account) + "/merchants/" + url.PathEscape(cmd.Merchant) + "/identity"
	expected := railcontract.ExpectedProcessorIdentity(ctx)
	if expected != "" && !ledgerPattern.MatchString(expected) {
		return "", railcontract.ErrProcessorIdentityMismatch
	}
	code, body, header, err := c.request(ctx, http.MethodGet, path, nil, expected)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("processor identity discovery returned HTTP %d", code)
	}
	var identity ledgerIdentity
	if decodeEvidence(body, &identity) != nil || identity != (ledgerIdentity{cmdRoute(cmd), header, cmd.Merchant, cmd.Account}) {
		return "", railcontract.ErrEvidenceConflict
	}
	return identity.ID, nil
}

var _ railcontract.ProcessorIdentity = (*Client)(nil)

// Package virtualrail runs an independent processor for synthetic terminal and
// hosted payments. Its command endpoint and its reader authentication are separate.
package virtualrail

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

type Program struct {
	RouteID         string     `json:"route_id,omitempty"`
	ConnectorID     string     `json:"connector_id,omitempty"`
	ProviderAccount string     `json:"provider_account,omitempty"`
	PaymentMethods  []string   `json:"payment_methods,omitempty"`
	Beneficiary     string     `json:"beneficiary,omitempty"`
	StatementToken  string     `json:"statement_token,omitempty"`
	Merchant        string     `json:"merchant"`
	Account         string     `json:"account"`
	Currency        string     `json:"currency"`
	MaxAmount       int64      `json:"max_amount"`
	Outlets         []string   `json:"outlets"`
	Terminals       []Terminal `json:"terminals"`
}

type Terminal struct {
	ID     string `json:"id"`
	Outlet string `json:"outlet"`
	Token  string `json:"token"`
}

type enrollment struct{ ID, Outlet, Merchant, Account string }
type program struct {
	Merchant, Account, Currency           string
	MaxAmount                             int64
	Outlets                               map[string]bool
	RouteID, ConnectorID, ProviderAccount string
	Methods                               map[string]bool
}
type service struct {
	db          *pgxpool.Pool
	origin      string
	ledgerID    string
	connector   [32]byte
	programs    map[string]program
	terminals   map[string]enrollment
	credentials map[[32]byte]enrollment
	exports     map[[32]byte]exportProgram
}

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var validCurrency = regexp.MustCompile(`^[A-Z]{3}$`)
var validCapability = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

type failure struct {
	code    int
	message string
}

func (e *failure) Error() string        { return e.message }
func reject(code int, msg string) error { return &failure{code, msg} }
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func writeFailure(w http.ResponseWriter, err error) {
	var f *failure
	if errors.As(err, &f) {
		writeJSON(w, f.code, map[string]string{"error": f.message})
		return
	}
	writeJSON(w, 503, map[string]string{"error": "Processor unavailable; query the original instruction."})
}
func randomID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
func bearer(h http.Header) string {
	values := h.Values("Authorization")
	if len(values) != 1 {
		return ""
	}
	if t, ok := strings.CutPrefix(values[0], "Bearer "); ok {
		return t
	}
	return ""
}
func readBody(r *http.Request, dst any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 8193))
	if err != nil || len(b) > 8192 {
		return reject(400, "Invalid or oversized body.")
	}
	if err = decodeEvidence(b, dst); err != nil {
		return reject(400, "Invalid request fields.")
	}
	return nil
}

func checkOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.ForceQuery {
		return nil, errors.New("invalid virtual processor origin")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()))) {
		return nil, errors.New("virtual processor requires HTTPS or loopback HTTP")
	}
	return u, nil
}

// New installs and verifies the processor's bindings. Only enrolled credentials
// in this process configuration may initiate a presentation. Reassignment or
// credential rotation cannot silently replace an existing enrollment on restart.
func New(ctx context.Context, db *pgxpool.Pool, origin, token string, programs []Program) (http.Handler, error) {
	u, err := checkOrigin(origin)
	if err != nil {
		return nil, err
	}
	if db == nil || len(token) < 16 || len(programs) == 0 {
		return nil, errors.New("supply processor database, connector credential and programs")
	}
	s := &service{db: db, origin: origin, connector: sha256.Sum256([]byte(token)), programs: map[string]program{}, terminals: map[string]enrollment{}, credentials: map[[32]byte]enrollment{}, exports: map[[32]byte]exportProgram{}}
	err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(84277015)"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, schema); err != nil {
			return err
		}
		s.ledgerID, err = installLedgerIdentity(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range programs {
			if !validID.MatchString(p.Merchant) || !validID.MatchString(p.Account) || !validCurrency.MatchString(p.Currency) || p.MaxAmount <= 0 || p.MaxAmount > 1<<53-1 || len(p.Outlets) == 0 {
				return errors.New("invalid virtual merchant program")
			}
			if _, exists := s.programs[p.Account]; exists {
				return errors.New("processor account must be unique")
			}
			cfg := program{Merchant: p.Merchant, Account: p.Account, Currency: p.Currency, MaxAmount: p.MaxAmount, Outlets: map[string]bool{}, RouteID: p.RouteID, ConnectorID: p.ConnectorID, ProviderAccount: p.ProviderAccount}
			cfg.Methods, err = checkMethods(p)
			if err != nil {
				return err
			}
			for _, outlet := range p.Outlets {
				if !validID.MatchString(outlet) || cfg.Outlets[outlet] {
					return errors.New("invalid or duplicate outlet")
				}
				cfg.Outlets[outlet] = true
			}
			s.programs[p.Account] = cfg
			if _, err := tx.Exec(ctx, "INSERT INTO virtualrail.program(merchant,account,currency) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING", p.Merchant, p.Account, p.Currency); err != nil {
				return err
			}
			var ok bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM virtualrail.program WHERE merchant=$1 AND account=$2 AND currency=$3)", p.Merchant, p.Account, p.Currency).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return errors.New("processor account binding differs from recorded enrollment")
			}
			if err := bindRoute(ctx, tx, p); err != nil {
				return err
			}
			if err := s.enrollExport(ctx, tx, p); err != nil {
				return err
			}
			for _, t := range p.Terminals {
				hash := sha256.Sum256([]byte(t.Token))
				if !validID.MatchString(t.ID) || !cfg.Outlets[t.Outlet] || len(t.Token) < 16 || hash == s.connector {
					return errors.New("invalid reader enrollment")
				}
				if _, ok := s.terminals[t.ID]; ok {
					return errors.New("duplicate reader ID")
				}
				if _, ok := s.exports[hash]; ok {
					return errors.New("reader credential also used for statement export")
				}
				if _, ok := s.credentials[hash]; ok {
					return errors.New("duplicate reader credential")
				}
				e := enrollment{t.ID, t.Outlet, p.Merchant, p.Account}
				s.terminals[t.ID] = e
				s.credentials[hash] = e
				if _, err := tx.Exec(ctx, "INSERT INTO virtualrail.terminal(id,merchant,account,outlet,credential_digest) VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING", t.ID, p.Merchant, p.Account, t.Outlet, hash[:]); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM virtualrail.terminal WHERE id=$1 AND merchant=$2 AND account=$3 AND outlet=$4 AND credential_digest=$5)", t.ID, p.Merchant, p.Account, t.Outlet, hash[:]).Scan(&ok); err != nil {
					return err
				}
				if !ok {
					return errors.New("reader binding differs from recorded enrollment")
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("initialize virtual rail: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /rail/v1/accounts/{account}/merchants/{merchant}/identity", s.requireConnector(s.identityHTTP))
	mux.HandleFunc("POST /rail/v1/operations", s.requireConnector(s.requireLedger(s.submitHTTP)))
	mux.HandleFunc("GET /rail/v1/accounts/{account}/merchants/{merchant}/operations/{id}", s.requireConnector(s.requireLedger(s.lookupHTTP)))
	mux.HandleFunc("GET /rail/v1/accounts/{account}/merchants/{merchant}/operations/{id}/release-evidence", s.requireConnector(s.requireLedger(s.releaseHTTP)))
	mux.HandleFunc("GET /device/v1/command", s.deviceHTTP)
	mux.HandleFunc("POST /device/v1/commands/{id}/decision", s.deviceHTTP)
	s.registerUI(mux)
	mux.HandleFunc("GET /rail/v1/remittances/{id}", s.remittanceHTTP)
	mux.HandleFunc("POST /rail/v1/remittances/{id}", s.remittanceHTTP)
	mux.HandleFunc("GET /rail/v1/remittances/{id}/source", s.remittanceHTTP)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(r.Context()); err != nil {
			writeFailure(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"service": "virtual-rail", "mode": "virtual", "status": "ready"})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		if r.Host != u.Host {
			writeJSON(w, 421, map[string]string{"error": "Unrecognized processor origin."})
			return
		}
		mux.ServeHTTP(w, r)
	}), nil
}

func (s *service) requireConnector(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(bearer(r.Header)))
		if subtle.ConstantTimeCompare(got[:], s.connector[:]) != 1 {
			writeJSON(w, 401, map[string]string{"error": "Processor credential required."})
			return
		}
		h(w, r)
	}
}

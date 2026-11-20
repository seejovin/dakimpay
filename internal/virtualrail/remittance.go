package virtualrail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type exportProgram struct{ Merchant, Account, Beneficiary, Currency string }

func (s *service) enrollExport(ctx context.Context, tx pgx.Tx, p Program) error {
	if p.StatementToken == "" && p.Beneficiary == "" {
		return nil
	}
	digest := sha256.Sum256([]byte(p.StatementToken))
	if !validID.MatchString(p.Beneficiary) || len(p.StatementToken) < 16 || digest == s.connector {
		return errors.New("invalid statement program credential or beneficiary")
	}
	if _, ok := s.credentials[digest]; ok {
		return errors.New("reader credential also used for export")
	}
	if _, ok := s.exports[digest]; ok {
		return errors.New("duplicate statement credential")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO virtualrail.export_program(merchant,account,beneficiary,credential_digest) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, p.Merchant, p.Account, p.Beneficiary, digest[:]); err != nil {
		return err
	}
	var same bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM virtualrail.export_program WHERE merchant=$1 AND account=$2 AND beneficiary=$3 AND credential_digest=$4)", p.Merchant, p.Account, p.Beneficiary, digest[:]).Scan(&same); err != nil {
		return err
	}
	if !same {
		return errors.New("statement account differs from enrolled program")
	}
	s.exports[digest] = exportProgram{p.Merchant, p.Account, p.Beneficiary, p.Currency}
	return nil
}

type nativeMovement struct {
	Method    string    `json:"payment_method,omitempty"`
	CardBrand string    `json:"card_brand,omitempty"`
	Operation string    `json:"operation_id"`
	Reference string    `json:"reference"`
	Kind      string    `json:"kind"`
	Channel   string    `json:"channel"`
	Amount    int64     `json:"amount_minor"`
	Fee       int64     `json:"processing_fee_minor"`
	Booked    time.Time `json:"booked_at"`
}
type nativeRemittance struct {
	routeIdentity
	Ledger      string           `json:"processor_ledger,omitempty"`
	Merchant    string           `json:"merchant_id"`
	Account     string           `json:"processor_account"`
	Currency    string           `json:"currency"`
	Beneficiary string           `json:"beneficiary"`
	ID          string           `json:"payout_id"`
	Trace       string           `json:"bank_trace"`
	Policy      string           `json:"fee_policy"`
	Movements   []nativeMovement `json:"movements"`
}
type settlementLine struct {
	ID        string `json:"line_id"`
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Parent    string `json:"parent_reference,omitempty"`
	Amount    int64  `json:"amount_minor"`
}
type settlementReport struct {
	RouteID         string           `json:"route_id,omitempty"`
	ProviderAccount string           `json:"provider_account,omitempty"`
	ID              string           `json:"payout_id"`
	Trace           string           `json:"bank_trace"`
	Beneficiary     string           `json:"beneficiary"`
	Currency        string           `json:"currency"`
	Net             int64            `json:"net_minor"`
	Source          string           `json:"source_digest"`
	Lines           []settlementLine `json:"lines"`
}

// This virtual program's contract fixes terminal at 2%, hosted at 2.5%,
// rounds half up in minor units, and charges no refund fee. All negative
// financial movements are source observations, not bank-confirmed deposits.
func sampleFee(m nativeMovement) int64 {
	if m.Kind != "capture" {
		return 0
	}
	divisor := int64(50)
	if m.Channel == "hosted" {
		divisor = 40
	}
	fee := m.Amount / divisor
	if (m.Amount%divisor)*2 >= divisor {
		fee++
	}
	return fee
}

func (s *service) freeze(ctx context.Context, p exportProgram, id string) (body []byte, created bool, err error) {
	err = s.ledgerTx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
		// Only competing cuts for this program serialize at this lock. Source
		// postings continue concurrently and appear here or in a later batch.
		if _, err := tx.Exec(ctx, "SELECT 1 FROM virtualrail.export_program WHERE merchant=$1 AND account=$2 FOR UPDATE", p.Merchant, p.Account); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, "SELECT report FROM virtualrail.remittance WHERE merchant=$1 AND account=$2 AND id=$3", p.Merchant, p.Account, id).Scan(&body)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Worst case: a capture and its fee consume two of 250 statement lines.
		rows, err := tx.Query(ctx, `SELECT m.operation_id,m.reference,m.kind,o.channel,m.amount,m.recorded_at,o.payment_method,o.card_brand
        FROM virtualrail.movement m JOIN virtualrail.operation o ON (o.merchant,o.account,o.id)=(m.merchant,m.account,m.operation_id)
        WHERE m.merchant=$1 AND m.account=$2 AND NOT EXISTS (SELECT 1 FROM virtualrail.allocation a WHERE a.movement=m.reference)
        ORDER BY m.recorded_at,m.reference LIMIT 125`, p.Merchant, p.Account)
		if err != nil {
			return err
		}
		movements, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (nativeMovement, error) {
			var m nativeMovement
			err := row.Scan(&m.Operation, &m.Reference, &m.Kind, &m.Channel, &m.Amount, &m.Booked, &m.Method, &m.CardBrand)
			m.Booked = m.Booked.UTC()
			m.Fee = sampleFee(m)
			return m, err
		})
		if err != nil {
			return err
		}
		if len(movements) == 0 {
			return reject(409, "No unallocated processor movements.")
		}
		randID, err := randomID()
		if err != nil {
			return err
		}
		trace := "vt_" + randID
		program := s.programs[p.Account]
		src := nativeRemittance{Merchant: p.Merchant, Account: p.Account, Currency: p.Currency, Beneficiary: p.Beneficiary, ID: id, Trace: trace, Policy: "virtual-card-v1", Movements: movements}
		if program.RouteID != "" {
			src.routeIdentity = routeIdentity{program.RouteID, program.ConnectorID, program.ProviderAccount}
			src.Ledger = s.ledgerID
			src.Policy = "virtual-processing-v1"
		}
		raw, err := json.Marshal(src)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(raw)
		report := settlementReport{RouteID: program.RouteID, ProviderAccount: program.ProviderAccount, ID: id, Trace: trace, Beneficiary: p.Beneficiary, Currency: p.Currency, Source: hex.EncodeToString(hash[:])}
		for _, m := range movements {
			report.Lines = append(report.Lines, settlementLine{ID: m.Reference, Kind: m.Kind, Reference: m.Reference, Amount: m.Amount})
			report.Net += m.Amount - m.Fee
			if m.Fee != 0 {
				feeID := "fee_" + m.Reference
				report.Lines = append(report.Lines, settlementLine{feeID, "fee", feeID, m.Reference, -m.Fee})
			}
		}
		// Program caps (<=2^53-1 minor units) and 125 source movements bound
		// the total magnitude below int64. One allocation owns each movement.
		body, err = json.Marshal(report)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO virtualrail.remittance(merchant,account,id,report,source) VALUES($1,$2,$3,$4,$5)", p.Merchant, p.Account, id, body, raw); err != nil {
			return err
		}
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"virtualrail", "allocation"}, []string{"movement", "merchant", "account", "remittance"}, pgx.CopyFromSlice(len(movements), func(i int) ([]any, error) { return []any{movements[i].Reference, p.Merchant, p.Account, id}, nil }))
		if err != nil {
			return err
		}
		created = true
		return nil
	})
	return
}
func (s *service) remittanceHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := s.exports[sha256.Sum256([]byte(bearer(r.Header)))]
	if !ok {
		writeFailure(w, reject(401, "Processor statement credential required."))
		return
	}
	id := r.PathValue("id")
	if !validID.MatchString(id) {
		writeFailure(w, reject(422, "Invalid payout reference."))
		return
	}
	var body []byte
	var err error
	status := 200
	if r.Method == http.MethodPost {
		if r.ContentLength != 0 {
			writeFailure(w, reject(400, "Supply only the new payout identifier."))
			return
		}
		var created bool
		body, created, err = s.freeze(r.Context(), p, id)
		if created {
			status = 201
		}
	} else {
		col := "report"
		if strings.HasSuffix(r.Pattern, "/source") {
			col = "source"
		}
		err = s.ledgerTx(r.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
			return tx.QueryRow(r.Context(), "SELECT "+col+" FROM virtualrail.remittance WHERE merchant=$1 AND account=$2 AND id=$3", p.Merchant, p.Account, id).Scan(&body)
		})
		if errors.Is(err, pgx.ErrNoRows) {
			err = reject(404, "Processor remittance not found.")
		}
	}
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

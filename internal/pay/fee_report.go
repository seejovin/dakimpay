package pay

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
)

type feeReportLink struct {
	Operation   string `json:"operation_id"`
	Batch       string `json:"batch"`
	Payout      string `json:"payout_id"`
	LocalStatus string `json:"local_status"`
}

const feeReportsSelect = `SELECT s.operation_id,s.batch,coalesce(p.id,''),coalesce(p.status,'unbooked')
 FROM pay.settlement_requests s LEFT JOIN pay.payouts p ON p.operation_id=s.operation_id WHERE NOT s.empty`

func readFeeReportLink(row pgx.Row) (feeReportLink, error) {
	var v feeReportLink
	err := row.Scan(&v.Operation, &v.Batch, &v.Payout, &v.LocalStatus)
	return v, err
}

type sourceFeeEstimate struct {
	Payment    string     `json:"payment_id"`
	Reference  string     `json:"reference"`
	Gross      *int64     `json:"amount"`
	Currency   string     `json:"currency"`
	Fee        *int64     `json:"fee"`
	Rate       *int64     `json:"rate_bps"`
	Policy     string     `json:"policy"`
	RecordedAt *time.Time `json:"recorded_at"`
}

type sourceFeeLine struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Amount    *int64 `json:"amount"`
}

type sourceFeeStatement struct {
	Operation string              `json:"operation_id"`
	Merchant  string              `json:"merchant_id"`
	Currency  string              `json:"currency"`
	Payout    string              `json:"payout_id"`
	Bank      string              `json:"bank_trace"`
	Net       *int64              `json:"net"`
	Lines     []sourceFeeLine     `json:"lines"`
	Estimates []sourceFeeEstimate `json:"estimates"`
}

func (s *sourceFeeStatement) UnmarshalJSON(raw []byte) error {
	type plain sourceFeeStatement
	// The connector already bounds the body size. Itemized statements can
	// legitimately contain many more lines than a single capture has refunds.
	return decodeBoundedEvidence(raw, (*plain)(s), len(raw))
}

type feeComparison struct {
	Payment    string     `json:"payment_id"`
	Order      string     `json:"order_id"`
	Reference  string     `json:"reference"`
	LineID     string     `json:"line_id"`
	Gross      int64      `json:"gross"`
	Expected   *int64     `json:"expected"`
	Actual     int64      `json:"actual"`
	Variance   *int64     `json:"variance"`
	Rate       *int64     `json:"rate_bps"`
	Policy     string     `json:"policy"`
	RecordedAt *time.Time `json:"recorded_at"`
}

type feeVarianceReport struct {
	Operation      string          `json:"operation_id"`
	Payout         string          `json:"payout_id"`
	Merchant       string          `json:"merchant_id"`
	Currency       string          `json:"currency"`
	Bank           string          `json:"bank_trace"`
	Net            int64           `json:"net"`
	BankStatus     string          `json:"bank_status"`
	BankAmount     *int64          `json:"bank_amount"`
	LocalStatus    string          `json:"local_status"`
	Status         string          `json:"status"`
	DifferentCount int             `json:"different_count"`
	MissingCount   int             `json:"missing_count"`
	ExpectedTotal  *int64          `json:"expected_total"`
	ActualTotal    int64           `json:"actual_total"`
	Variance       *int64          `json:"variance"`
	Lines          []feeComparison `json:"lines"`
}

type movementKey struct{ kind, reference string }
type feeStatementIndex struct {
	lines    map[movementKey]Line
	quotes   map[string]sourceFeeEstimate
	captures []string
}

func invalidFeeEvidence() error {
	return fail(409, "The itemized processor evidence is incomplete or inconsistent with the original settlement.")
}

// Simulator payments are bounded at one million USD, in cents. Statement
// movement bounds also keep totals exact within the connector's response size.
const maxFeeMovement int64 = 100_000_000

func (s sourceFeeStatement) index(operation string) (feeStatementIndex, error) {
	idx := feeStatementIndex{lines: make(map[movementKey]Line), quotes: make(map[string]sourceFeeEstimate)}
	if s.Operation != operation || s.Merchant != "mimi" || s.Currency != "USD" || !identifier.MatchString(s.Payout) ||
		!identifier.MatchString(s.Bank) || s.Net == nil || len(s.Lines) == 0 || s.Estimates == nil {
		return idx, invalidFeeEvidence()
	}
	ids := make(map[string]bool)
	var total int64
	for _, l := range s.Lines {
		key := movementKey{l.Kind, l.Reference}
		if !identifier.MatchString(l.ID) || !identifier.MatchString(l.Reference) || ids[l.ID] || idx.lines[key].ID != "" || l.Amount == nil ||
			*l.Amount < -maxFeeMovement || *l.Amount > maxFeeMovement {
			return idx, invalidFeeEvidence()
		}
		amount := *l.Amount
		switch l.Kind {
		case "capture":
			if amount <= 0 {
				return idx, invalidFeeEvidence()
			}
			idx.captures = append(idx.captures, l.Reference)
		case "refund":
			if amount >= 0 {
				return idx, invalidFeeEvidence()
			}
		case "fee":
			if amount > 0 {
				return idx, invalidFeeEvidence()
			}
		default:
			return idx, invalidFeeEvidence()
		}
		idx.lines[key] = Line{ID: l.ID, Kind: l.Kind, Reference: l.Reference, Amount: amount}
		ids[l.ID] = true
		total += amount
	}
	if total != *s.Net {
		return idx, invalidFeeEvidence()
	}
	for k := range idx.lines {
		if k.kind == "fee" && idx.lines[movementKey{"capture", k.reference}].ID == "" ||
			k.kind == "capture" && idx.lines[movementKey{"fee", k.reference}].ID == "" {
			return idx, invalidFeeEvidence()
		}
	}
	for _, q := range s.Estimates {
		capture := idx.lines[movementKey{"capture", q.Reference}]
		if capture.ID == "" || q.Gross == nil || *q.Gross != capture.Amount || q.Currency != s.Currency || !identifier.MatchString(q.Payment) ||
			!identifier.MatchString(q.Policy) || q.RecordedAt == nil || q.RecordedAt.IsZero() || q.Rate == nil || *q.Rate < 0 || *q.Rate > 10000 ||
			q.Fee == nil || *q.Fee < 0 || *q.Fee > maxFeeMovement || idx.quotes[q.Reference].Payment != "" {
			return idx, invalidFeeEvidence()
		}
		idx.quotes[q.Reference] = q
	}
	return idx, nil
}

// The acquirer may include new captures after our local cutoff. Check the
// minimum expected set, scope of every refund and reuse of prior movements.
func verifyReportMovements(ctx context.Context, tx pgx.Tx, link feeReportLink, f sourceFeeStatement) error {
	var kinds, refs, ids []string
	var amounts []int64
	for _, line := range f.Lines {
		kinds = append(kinds, line.Kind)
		refs = append(refs, line.Reference)
		ids = append(ids, line.ID)
		amounts = append(amounts, *line.Amount)
	}
	var invalid bool
	err := tx.QueryRow(ctx, `WITH actual AS (
	    SELECT * FROM unnest($1::text[],$2::text[],$3::bigint[],$9::text[]) AS t(kind,reference,amount,id)
	) SELECT EXISTS(
	    SELECT kind,reference FROM pay.settlement_expected WHERE operation_id=$4
	    EXCEPT SELECT kind,reference FROM actual
	) OR EXISTS(
	    SELECT FROM actual a JOIN pay.lines l USING(kind,reference) WHERE l.payout_id<>$5
	) OR EXISTS(
	    SELECT FROM actual a JOIN pay.lines l USING(id) WHERE l.payout_id<>$5
	) OR EXISTS(
	    SELECT FROM pay.payouts WHERE (id=$5 OR bank_trace=$8) AND operation_id<>$4
	) OR EXISTS(
	    SELECT FROM actual a WHERE a.kind='refund' AND NOT EXISTS(
	        SELECT FROM pay.refunds r JOIN pay.payments p ON p.id=r.payment_id
	        WHERE r.reference=a.reference AND r.status='succeeded' AND r.amount=-a.amount
	          AND r.currency=$6 AND p.merchant_id='mimi' AND p.currency=r.currency
	          AND ((p.order_id IN ('MIMI-GUIDED-TERMINAL','MIMI-GUIDED-ONLINE'))=($7='guided'))
	    )
	)`, kinds, refs, amounts, f.Operation, f.Payout, f.Currency, link.Batch, f.Bank, ids).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid {
		return invalidFeeEvidence()
	}
	return nil
}

// Read original merchant records in one snapshot. The fee diagnostic never
// invokes settlement, discovers new refunds, or writes a bank match.
func (s *Service) compareFees(ctx context.Context, f sourceFeeStatement, idx feeStatementIndex) (feeVarianceReport, error) {
	report := feeVarianceReport{Operation: f.Operation, Payout: f.Payout, Merchant: f.Merchant, Currency: f.Currency, Bank: f.Bank, Net: *f.Net, Lines: []feeComparison{}}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return report, err
	}
	defer tx.Rollback(ctx)
	link, err := readFeeReportLink(tx.QueryRow(ctx, feeReportsSelect+" AND s.operation_id=$1", f.Operation))
	if err != nil {
		return report, err
	}
	report.LocalStatus = link.LocalStatus
	if link.Payout != "" {
		p, err := readPayout(tx.QueryRow(ctx, "SELECT "+payoutColumns+" FROM pay.payouts b WHERE b.id=$1", link.Payout))
		if err != nil {
			return report, err
		}
		if p.ID != f.Payout || p.BankTrace != f.Bank || p.Net != *f.Net || p.Currency != f.Currency || len(p.Lines) != len(f.Lines) {
			return report, invalidFeeEvidence()
		}
		for _, l := range p.Lines {
			if idx.lines[movementKey{l.Kind, l.Reference}] != l {
				return report, invalidFeeEvidence()
			}
		}
	}
	if err = verifyReportMovements(ctx, tx, link, f); err != nil {
		return report, err
	}
	rows, err := tx.Query(ctx, `SELECT p.id,p.order_id,p.reference,p.amount FROM pay.payments p
	   WHERE p.merchant_id='mimi' AND p.status='captured' AND p.currency=$2 AND p.reference=ANY($1)
	   AND ((p.order_id IN ('MIMI-GUIDED-TERMINAL','MIMI-GUIDED-ONLINE'))=($3='guided')) ORDER BY p.reference`, idx.captures, f.Currency, link.Batch)
	if err != nil {
		return report, err
	}
	report.Lines, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (feeComparison, error) {
		var l feeComparison
		if err := row.Scan(&l.Payment, &l.Order, &l.Reference, &l.Gross); err != nil {
			return l, err
		}
		capture := idx.lines[movementKey{"capture", l.Reference}]
		actual := idx.lines[movementKey{"fee", l.Reference}]
		if l.Gross != capture.Amount {
			return l, invalidFeeEvidence()
		}
		l.LineID, l.Actual = actual.ID, -actual.Amount
		if q, ok := idx.quotes[l.Reference]; ok {
			if q.Payment != l.Payment {
				return l, invalidFeeEvidence()
			}
			diff := l.Actual - *q.Fee
			l.Expected, l.Variance, l.Rate, l.Policy, l.RecordedAt = q.Fee, &diff, q.Rate, q.Policy, q.RecordedAt
		}
		return l, nil
	})
	if err != nil {
		return report, err
	}
	if len(report.Lines) != len(idx.captures) {
		return report, invalidFeeEvidence()
	}
	if err = tx.Commit(ctx); err != nil {
		return report, err
	}
	report.summarize()
	return report, nil
}

func (r *feeVarianceReport) summarize() {
	var expected, variance int64
	for _, l := range r.Lines {
		r.ActualTotal += l.Actual
		if l.Expected == nil {
			r.MissingCount++
			continue
		}
		expected += *l.Expected
		variance += *l.Variance
		if *l.Variance != 0 {
			r.DifferentCount++
		}
	}
	if r.MissingCount > 0 {
		r.Status = "incomplete"
		return
	}
	r.ExpectedTotal, r.Variance = &expected, &variance
	switch {
	case len(r.Lines) == 0:
		r.Status = "no_fees"
	case r.DifferentCount > 0:
		r.Status = "variance"
	default:
		r.Status = "aligned"
	}
}

type feeBankObservation struct {
	Trace    string `json:"trace"`
	Merchant string `json:"merchant_id"`
	Currency string `json:"currency"`
	Amount   *int64 `json:"amount"`
	Status   string `json:"status"`
}

func (b *feeBankObservation) UnmarshalJSON(raw []byte) error {
	type plain feeBankObservation
	return decodeBoundedEvidence(raw, (*plain)(b), 0)
}

func (s *Service) feeReport(w http.ResponseWriter, r *http.Request) error {
	ctx, op := r.Context(), r.PathValue("id")
	// Confirm ownership before asking either independent service for evidence.
	if _, err := readFeeReportLink(s.db.QueryRow(ctx, feeReportsSelect+" AND s.operation_id=$1", op)); err != nil {
		return err
	}
	var f sourceFeeStatement
	code, err := s.remote(ctx, s.acquirer, http.MethodGet, "/sim/payouts/"+url.PathEscape(op)+"/fee-evidence", nil, &f)
	if code == 404 {
		return fail(409, "The processor has no frozen statement for this settlement yet.")
	}
	if err != nil {
		return fail(502, "The processor's itemized fee evidence is unavailable or unreadable.")
	}
	idx, err := f.index(op)
	if err != nil {
		return err
	}
	report, err := s.compareFees(ctx, f, idx)
	if err != nil {
		return err
	}
	var b feeBankObservation
	code, err = s.remote(ctx, s.bank, http.MethodGet, "/bank/entries/"+url.PathEscape(f.Bank), nil, &b)
	switch {
	case code == 404:
		report.BankStatus = "pending"
	case err != nil:
		report.BankStatus = "unavailable"
	case b.Trace != f.Bank || b.Merchant != f.Merchant || b.Currency != f.Currency || b.Status != "posted" || b.Amount == nil:
		report.BankStatus = "mismatch"
	default:
		report.BankAmount = b.Amount
		report.BankStatus = "matched"
		if *b.Amount != report.Net {
			report.BankStatus = "mismatch"
		}
	}
	answer(w, http.StatusOK, report)
	return nil
}

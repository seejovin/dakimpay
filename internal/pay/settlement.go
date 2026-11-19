package pay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
)

type Line struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Amount    int64  `json:"amount"`
}
type Payout struct {
	ID         string    `json:"id"`
	MerchantID string    `json:"merchant_id"`
	Currency   string    `json:"currency"`
	BankTrace  string    `json:"bank_trace"`
	Net        int64     `json:"net"`
	Lines      []Line    `json:"lines"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

const payoutColumns = `b.id,'mimi' AS merchant_id,b.currency,b.bank_trace,b.net,b.status,b.created_at,coalesce((SELECT jsonb_agg(jsonb_build_object('id',l.id,'reference',l.reference,'kind',l.kind,'amount',l.amount) ORDER BY l.id) FROM pay.lines l WHERE l.payout_id=b.id),'[]')`

func readPayout(row pgx.Row) (Payout, error) {
	var p Payout
	var raw []byte
	err := row.Scan(&p.ID, &p.MerchantID, &p.Currency, &p.BankTrace, &p.Net, &p.Status, &p.CreatedAt, &raw)
	if err == nil {
		err = json.Unmarshal(raw, &p.Lines)
	}
	return p, err
}
func (s *Service) settle(w http.ResponseWriter, r *http.Request) error {
	k, err := key(r)
	if err != nil {
		return err
	}
	var input struct {
		Batch string `json:"batch"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err = decode(r, &input); err != nil {
			return err
		}
	}
	if input.Batch == "" {
		input.Batch = "standard"
	}
	if input.Batch != "standard" && input.Batch != "guided" {
		return fail(422, "Unknown settlement batch.")
	}
	ctx := r.Context()
	operation, err := s.settlementOperation(ctx, k, input.Batch)
	if err != nil {
		return err
	}
	batch := input.Batch
	existing, err := readPayout(s.db.QueryRow(ctx, "SELECT "+payoutColumns+" FROM pay.payouts b WHERE b.operation_id=$1", operation))
	if err == nil {
		answer(w, 200, existing)
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var fact Payout
	upstream, err := s.remote(ctx, s.acquirer, http.MethodPost, "/sim/payouts", map[string]string{"operation_id": operation, "merchant_id": "mimi", "currency": "USD", "batch": batch}, &fact)
	if err != nil {
		if upstream == 409 {
			tag, e := s.db.Exec(ctx, `UPDATE pay.settlement_requests SET empty=true WHERE operation_id=$1 AND NOT EXISTS(SELECT 1 FROM pay.settlement_expected WHERE operation_id=$1)`, operation)
			if e != nil {
				return e
			}
			if tag.RowsAffected() == 0 {
				return fail(409, "The acquirer reported no lines for known unsettled operations. Keep this batch open for review.")
			}
			return fail(409, "There are no eligible acquirer statement lines in this batch.")
		}
		return fail(502, "Statement unavailable. Retry the original settlement request.")
	}
	if fact.MerchantID != "mimi" || fact.Currency != "USD" || !identifier.MatchString(fact.ID) || !identifier.MatchString(fact.BankTrace) || len(fact.Lines) == 0 {
		return fail(502, "Invalid acquirer statement identity or currency.")
	}
	// The statement can arrive before its notification. Discover any unknown
	// refund by source reference and verify it independently before matching.
	if err = s.discoverStatementRefunds(ctx, fact.Lines); err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Serializes concurrent attempts to store the same original remittance.
	if _, err = tx.Exec(ctx, "SELECT operation_id FROM pay.settlement_requests WHERE operation_id=$1 FOR UPDATE", operation); err != nil {
		return err
	}
	existing, err = readPayout(tx.QueryRow(ctx, "SELECT "+payoutColumns+" FROM pay.payouts b WHERE b.operation_id=$1", operation))
	if err == nil {
		answer(w, 200, existing)
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO pay.payouts(id,operation_id,bank_trace,currency,net,status) VALUES($1,$2,$3,$4,$5,'in_transit')", fact.ID, operation, fact.BankTrace, fact.Currency, fact.Net); err != nil {
		return err
	}
	var sum int64
	for _, l := range fact.Lines {
		if err = matchLine(ctx, tx, l, batch); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO pay.lines(id,payout_id,kind,reference,amount) VALUES($1,$2,$3,$4,$5)", l.ID, fact.ID, l.Kind, l.Reference, l.Amount); err != nil {
			return err
		}
		sum += l.Amount
	}
	var missing bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(
 SELECT kind,reference FROM pay.settlement_expected WHERE operation_id=$1
 EXCEPT SELECT kind,reference FROM pay.lines WHERE payout_id=$2)`, operation, fact.ID).Scan(&missing); err != nil {
		return err
	}
	if missing {
		return fail(409, "Acquirer statement omitted a previously confirmed capture, fee or refund. Keep this batch open for review.")
	}
	// The simulator's agreement pairs each capture with exactly one fee in this payout.
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT reference FROM pay.lines WHERE payout_id=$1 AND kind IN ('capture','fee') GROUP BY reference HAVING count(*) <> 2)`, fact.ID).Scan(&missing); err != nil {
		return err
	}
	if missing {
		return fail(409, "Acquirer statement separated a capture from its contracted processing fee.")
	}
	if sum != fact.Net {
		return fail(409, "Statement lines do not equal the remittance. The payout is held for review.")
	}
	if err = note(ctx, tx, fact.ID, "in_transit", "Acquirer statement matched. Bank deposit is awaiting independent confirmation."); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	p, err := readPayout(s.db.QueryRow(ctx, "SELECT "+payoutColumns+" FROM pay.payouts b WHERE b.id=$1", fact.ID))
	if err != nil {
		return err
	}
	answer(w, 201, p)
	return nil
}

func (s *Service) discoverStatementRefunds(ctx context.Context, lines []Line) error {
	var refs []string
	for _, l := range lines {
		if l.Kind == "refund" {
			if !identifier.MatchString(l.Reference) {
				return fail(502, "Invalid statement refund reference.")
			}
			refs = append(refs, l.Reference)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	rows, err := s.db.Query(ctx, `SELECT DISTINCT source_ref FROM unnest($1::text[]) AS input(source_ref)
		WHERE NOT EXISTS(SELECT FROM pay.refunds r WHERE r.reference=input.source_ref AND r.status='succeeded')
		ORDER BY source_ref`, refs)
	if err != nil {
		return err
	}
	missing, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, reference := range missing {
		status, detail, err := s.inspectSourceRefund(ctx, "", reference)
		if err != nil {
			return err
		}
		if status != "applied" {
			return fail(409, detail)
		}
	}
	return nil
}
func matchLine(ctx context.Context, tx pgx.Tx, l Line, batch string) error {
	if !identifier.MatchString(l.ID) || !identifier.MatchString(l.Reference) {
		return fail(502, "Invalid acquirer statement line reference.")
	}
	var amount int64
	var order, channel string
	var err error
	switch l.Kind {
	case "capture", "fee":
		err = tx.QueryRow(ctx, "SELECT amount,order_id,channel FROM pay.payments WHERE reference=$1 AND merchant_id='mimi' AND currency='USD' AND status='captured'", l.Reference).Scan(&amount, &order, &channel)
	case "refund":
		err = tx.QueryRow(ctx, "SELECT r.amount,p.order_id,p.channel FROM pay.refunds r JOIN pay.payments p ON p.id=r.payment_id WHERE r.reference=$1 AND r.status='succeeded' AND p.merchant_id='mimi' AND r.currency='USD'", l.Reference).Scan(&amount, &order, &channel)
	default:
		return fail(409, "Unrecognized acquirer statement line type.")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fail(409, "A statement line has no verified original charge or refund. Recover it and retry the original statement.")
	}
	if err != nil {
		return err
	}
	guided := order == "MIMI-GUIDED-TERMINAL" || order == "MIMI-GUIDED-ONLINE"
	if guided != (batch == "guided") {
		return fail(409, "Statement entry belongs to a different batch.")
	}
	switch l.Kind {
	case "capture":
		if l.Amount == amount {
			return nil
		}
	case "refund":
		if l.Amount == -amount {
			return nil
		}
	case "fee":
		rate := int64(200)
		if channel == "online" {
			rate = 250
		}
		expected := (amount*rate + 5000) / 10000
		if l.Amount == -expected {
			return nil
		}
	}
	return fail(409, "Statement amount differs from its original payment or refund.")
}

type bankEntry struct {
	Trace      string `json:"trace"`
	MerchantID string `json:"merchant_id"`
	Currency   string `json:"currency"`
	Amount     int64  `json:"amount"`
	Status     string `json:"status"`
}

func (s *Service) reconcile(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	rows, err := s.db.Query(ctx, "SELECT id,bank_trace,currency,net FROM pay.payouts WHERE status='in_transit' ORDER BY created_at")
	if err != nil {
		return err
	}
	payouts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Payout, error) {
		var p Payout
		err := row.Scan(&p.ID, &p.BankTrace, &p.Currency, &p.Net)
		return p, err
	})
	if err != nil {
		return err
	}
	matched := 0
	for _, p := range payouts {
		var b bankEntry
		code, e := s.remote(ctx, s.bank, http.MethodGet, "/bank/entries/"+url.PathEscape(p.BankTrace), nil, &b)
		if code == 404 {
			continue
		}
		if e != nil {
			return fail(502, "Bank posting service unavailable. Unmatched payouts remain open.")
		}
		if b.Trace != p.BankTrace || b.Amount != p.Net || b.Currency != p.Currency || b.MerchantID != "mimi" || b.Status != "posted" {
			return fail(409, "Bank evidence differs from the remittance. The payout remains open.")
		}
		applied, e := s.matchBank(ctx, p, b)
		if e != nil {
			return e
		}
		if applied {
			matched++
		}
	}
	var pending int
	if err = s.db.QueryRow(ctx, "SELECT count(*) FROM pay.payouts WHERE status='in_transit'").Scan(&pending); err != nil {
		return err
	}
	answer(w, 200, map[string]int{"matched": matched, "pending": pending})
	return nil
}
func (s *Service) matchBank(ctx context.Context, p Payout, b bankEntry) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	evidence, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, "UPDATE pay.payouts SET status='matched',bank_evidence=$2 WHERE id=$1 AND status='in_transit'", p.ID, evidence)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err = note(ctx, tx, p.ID, "matched", "Independent bank posting matched the remittance trace, merchant, currency and net amount."); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// Freeze the minimum expected contents before requesting a simulator sweep. New
// concurrent captures may also appear if the acquirer accepted them by its cutoff.
// An interrupted remittance is recoverable even if the operator lost the first key.
func (s *Service) settlementOperation(ctx context.Context, key, batch string) (string, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('dakim-settle:' || $1))", batch); err != nil {
		return "", err
	}
	var op, originalBatch string
	var empty bool
	err = tx.QueryRow(ctx, `SELECT s.operation_id,s.batch,s.empty FROM pay.settlement_requests s
 WHERE s.request_key=$1 OR s.operation_id=(SELECT operation_id FROM pay.settlement_keys WHERE request_key=$1)`, key).Scan(&op, &originalBatch, &empty)
	if err == nil {
		if originalBatch != batch {
			return "", fail(409, "Settlement key is bound to another batch.")
		}
		if empty {
			return "", fail(409, "This original settlement request found no eligible statement lines.")
		}
		return op, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	err = tx.QueryRow(ctx, `SELECT s.operation_id FROM pay.settlement_requests s LEFT JOIN pay.payouts p ON p.operation_id=s.operation_id WHERE s.batch=$1 AND NOT s.empty AND p.id IS NULL ORDER BY s.operation_id LIMIT 1`, batch).Scan(&op)
	if errors.Is(err, pgx.ErrNoRows) {
		op = newID("ds")
		if _, err = tx.Exec(ctx, "INSERT INTO pay.settlement_requests(request_key,operation_id,batch) VALUES($1,$2,$3)", key, op, batch); err != nil {
			return "", err
		}
		_, err = tx.Exec(ctx, `WITH charges AS (SELECT p.reference,p.id FROM pay.payments p WHERE p.status='captured' AND p.merchant_id='mimi'
    AND ((p.order_id IN ('MIMI-GUIDED-TERMINAL','MIMI-GUIDED-ONLINE'))=($2='guided'))),
   evidence AS (SELECT k.kind,c.reference FROM charges c CROSS JOIN (VALUES ('capture'),('fee')) k(kind)
     UNION ALL SELECT 'refund',r.reference FROM pay.refunds r JOIN charges c ON c.id=r.payment_id WHERE r.status='succeeded')
   INSERT INTO pay.settlement_expected(operation_id,kind,reference)
   SELECT $1,e.kind,e.reference FROM evidence e WHERE NOT EXISTS(SELECT FROM pay.lines l WHERE l.kind=e.kind AND l.reference=e.reference)`, op, batch)
	}
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO pay.settlement_keys(request_key,operation_id) VALUES($1,$2)", key, op); err != nil {
		return "", err
	}
	return op, tx.Commit(ctx)
}

type merchantLine struct {
	Reference string `json:"reference"`
	Gross     int64  `json:"grossMinor"`
	Fee       int64  `json:"feeMinor"`
	Kind      string `json:"kind,omitempty"`
	Original  string `json:"original,omitempty"`
}

func (s *Service) merchantStatement(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	p, err := readPayout(tx.QueryRow(ctx, "SELECT "+payoutColumns+" FROM pay.payouts b WHERE b.id=$1", r.PathValue("id")))
	if err != nil {
		return err
	}
	if p.Status != "matched" {
		return fail(409, "A posted bank match is required before exporting a merchant deposit.")
	}
	rows, err := tx.Query(ctx, `SELECT coalesce(p.id,r.id) AS reference,
 sum(CASE WHEN l.kind='fee' THEN 0 ELSE l.amount END)::bigint AS gross,
 -sum(CASE WHEN l.kind='fee' THEN l.amount ELSE 0 END)::bigint AS fee,
 coalesce(max(CASE WHEN r.source_operation IS NOT NULL THEN 'provider_refund' END),''),
 coalesce(max(CASE WHEN r.source_operation IS NOT NULL THEN r.payment_id END),'')
 FROM pay.lines l LEFT JOIN pay.payments p ON p.merchant_id='mimi' AND l.kind IN ('capture','fee') AND p.reference=l.reference
 LEFT JOIN pay.refunds r ON l.kind='refund' AND r.reference=l.reference
 WHERE l.payout_id=$1 GROUP BY coalesce(p.id,r.id) ORDER BY coalesce(p.id,r.id)`, p.ID)
	if err != nil {
		return err
	}
	lines, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (merchantLine, error) {
		var l merchantLine
		err := row.Scan(&l.Reference, &l.Gross, &l.Fee, &l.Kind, &l.Original)
		return l, err
	})
	if err != nil {
		return err
	}
	w.Header().Set("Content-Disposition", `attachment; filename="dakim-pay-`+p.ID+`.json"`)
	answer(w, 200, map[string]any{"payout": p.ID, "bank": p.BankTrace, "currency": p.Currency, "depositMinor": p.Net, "lines": lines})
	return nil
}

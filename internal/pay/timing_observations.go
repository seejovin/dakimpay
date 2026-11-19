package pay

import (
	"context"
	_ "embed"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed timing_observations.sql
var timingObservationSchema string

type timingCheckpoint struct {
	KnownAt          time.Time
	EffectiveThrough time.Time
}

type timingFactKey struct{ source, trace string }

type timingFact struct {
	source, trace, operation, payout string
	amount                           int64
	effectiveAt, firstKnownAt        time.Time
}

func (f timingFact) sameOriginal(g timingFact) bool {
	return f.source == g.source && f.trace == g.trace && f.operation == g.operation && f.payout == g.payout &&
		f.amount == g.amount && f.effectiveAt.Equal(g.effectiveAt)
}

func observationFacts(cutoff time.Time, rows map[string]*timingRow) (map[timingFactKey]timingFact, error) {
	facts := make(map[timingFactKey]timingFact, len(rows))
	for trace, r := range rows {
		if r == nil || r.Trace != trace || !identifier.MatchString(trace) || r.Currency != "USD" ||
			(r.PayoutAt == nil) != (r.ProcessorNet == nil) || (r.PostedAt == nil) != (r.BankAmount == nil) ||
			(r.PayoutAt == nil && r.PostedAt == nil) {
			return nil, invalidTimingEvidence()
		}
		add := func(source string, stamp time.Time, amount int64, op, payout string) error {
			// The simulator's source clocks and coverage boundaries are native
			// PostgreSQL microseconds. Reject finer original timestamps rather
			// than silently changing the evidence during archival.
			if stamp.IsZero() || stamp.After(cutoff) || stamp.Nanosecond()%1000 != 0 {
				return invalidTimingEvidence()
			}
			facts[timingFactKey{source, trace}] = timingFact{source: source, trace: trace, operation: op, payout: payout, amount: amount, effectiveAt: stamp.UTC()}
			return nil
		}
		if r.PayoutAt != nil {
			if !identifier.MatchString(r.Operation) || !identifier.MatchString(r.Payout) {
				return nil, invalidTimingEvidence()
			}
			if err := add("processor", *r.PayoutAt, *r.ProcessorNet, r.Operation, r.Payout); err != nil {
				return nil, err
			}
		}
		if r.PostedAt != nil {
			if err := add("bank", *r.PostedAt, *r.BankAmount, "", ""); err != nil {
				return nil, err
			}
		}
		if r.BookedMatchAt != nil {
			if r.PayoutAt == nil || r.PostedAt == nil || *r.ProcessorNet != *r.BankAmount ||
				r.BookedMatchAt.Before(*r.PayoutAt) || r.BookedMatchAt.Before(*r.PostedAt) {
				return nil, invalidTimingEvidence()
			}
			if err := add("booking", *r.BookedMatchAt, *r.BankAmount, r.Operation, r.Payout); err != nil {
				return nil, err
			}
		}
	}
	return facts, nil
}

const timingFactColumns = "source,bank_trace,operation_id,payout_id,amount,effective_at,first_known_at"

func scanTimingFact(row pgx.Row) (timingFact, error) {
	var f timingFact
	err := row.Scan(&f.source, &f.trace, &f.operation, &f.payout, &f.amount, &f.effectiveAt, &f.firstKnownAt)
	return f, err
}

// Both locks are scoped to the reporting timeline. Readers use READ COMMITTED
// so the evidence query after locking sees the writer that just committed.
// No lock is held while fetching remote evidence or writing financial records.
const lockObservationWriter = "SELECT pg_advisory_xact_lock(hashtextextended('pay:timing-observations:mimi:USD',0))"
const lockObservationReader = "SELECT pg_advisory_xact_lock_shared(hashtextextended('pay:timing-observations:mimi:USD',0))"

func (s *Service) recordTimingObservation(ctx context.Context, sourceCutoff time.Time, rows map[string]*timingRow, clock func() time.Time) (time.Time, error) {
	var none time.Time
	if rows == nil || sourceCutoff.IsZero() || sourceCutoff.Nanosecond()%1000 != 0 {
		return none, invalidTimingEvidence()
	}
	incoming, err := observationFacts(sourceCutoff, rows)
	if err != nil {
		return none, err
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return none, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, lockObservationWriter); err != nil {
		return none, err
	}
	var last timingCheckpoint
	err = tx.QueryRow(ctx, `SELECT known_at,effective_through FROM pay.timing_observations ORDER BY known_at DESC LIMIT 1`).Scan(&last.KnownAt, &last.EffectiveThrough)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return none, err
	}
	if sourceCutoff.Before(last.EffectiveThrough) {
		return none, fail(409, "A later source cutoff is already recorded. Refresh the complete inventories.")
	}
	// A complete newer inventory must still include every previously known
	// immutable original. Remove known identical entries, leaving only the
	// new observations to publish. Missing and revised facts both roll back.
	prior, err := tx.Query(ctx, "SELECT "+timingFactColumns+" FROM pay.timing_facts")
	if err != nil {
		return none, err
	}
	for prior.Next() {
		f, err := scanTimingFact(prior)
		if err != nil {
			prior.Close()
			return none, err
		}
		key := timingFactKey{f.source, f.trace}
		if v, ok := incoming[key]; !ok || !f.sameOriginal(v) {
			prior.Close()
			return none, fail(409, "A previously recorded timing fact is missing or has changed. The observation was not recorded.")
		}
		delete(incoming, key)
	}
	if err = prior.Err(); err != nil {
		return none, err
	}
	if len(incoming) == 0 && sourceCutoff.Equal(last.EffectiveThrough) {
		return last.KnownAt.UTC(), nil
	}

	known := clock().UTC().Truncate(time.Microsecond)
	if known.Before(sourceCutoff) || !known.After(last.KnownAt) {
		return none, fail(409, "The reporting clock has not advanced beyond the last observation and its source cutoff. Retry the capture.")
	}
	if _, err = tx.Exec(ctx, "INSERT INTO pay.timing_observations(known_at,effective_through) VALUES($1,$2)", known, sourceCutoff); err != nil {
		return none, err
	}
	if len(incoming) > 0 {
		values := make([][]any, 0, len(incoming))
		for _, v := range incoming {
			values = append(values, []any{v.source, v.trace, v.operation, v.payout, v.amount, v.effectiveAt, known})
		}
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"pay", "timing_facts"}, []string{"source", "bank_trace", "operation_id", "payout_id", "amount", "effective_at", "first_known_at"}, pgx.CopyFromRows(values))
		if err != nil {
			return none, reject(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return none, err
	}
	return known, nil
}

// Reconstruct an answer only from facts published in a complete observation
// that finance could have seen by knownAsOf. Never consult live sources here.
func (s *Service) knownTimingRows(ctx context.Context, effectiveAsOf, knownAsOf time.Time) (map[string]*timingRow, timingCheckpoint, error) {
	var checkpoint timingCheckpoint
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, checkpoint, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, lockObservationReader); err != nil {
		return nil, checkpoint, err
	}
	err = tx.QueryRow(ctx, `SELECT known_at,effective_through FROM pay.timing_observations WHERE known_at<=$1 ORDER BY known_at DESC LIMIT 1`, knownAsOf).Scan(&checkpoint.KnownAt, &checkpoint.EffectiveThrough)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, checkpoint, fail(409, "No complete timing observation had been recorded by that knowledge cutoff.")
	}
	if err != nil {
		return nil, checkpoint, err
	}
	checkpoint.KnownAt, checkpoint.EffectiveThrough = checkpoint.KnownAt.UTC(), checkpoint.EffectiveThrough.UTC()
	result, err := tx.Query(ctx, "SELECT "+timingFactColumns+" FROM pay.timing_facts WHERE first_known_at<=$1 AND effective_at<=$2", checkpoint.KnownAt, effectiveAsOf)
	if err != nil {
		return nil, checkpoint, err
	}
	facts, err := pgx.CollectRows(result, func(row pgx.CollectableRow) (timingFact, error) { return scanTimingFact(row) })
	if err != nil {
		return nil, checkpoint, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, checkpoint, err
	}
	var payouts []processorTiming
	var postings []bankTiming
	for _, f := range facts {
		m := timingMovement{Merchant: "mimi", Currency: "USD", Trace: f.trace, Amount: &f.amount}
		switch f.source {
		case "processor":
			payouts = append(payouts, processorTiming{timingMovement: m, Operation: f.operation, Payout: f.payout, At: f.effectiveAt})
		case "bank":
			postings = append(postings, bankTiming{timingMovement: m, At: f.effectiveAt})
		}
	}
	joined, err := timingRows(effectiveAsOf, payouts, postings)
	if err != nil {
		return nil, checkpoint, err
	}
	for _, f := range facts {
		r := joined[f.trace]
		if r == nil {
			return nil, checkpoint, invalidTimingEvidence()
		}
		known := f.firstKnownAt.UTC()
		switch f.source {
		case "processor":
			r.PayoutKnownAt = &known
		case "bank":
			r.BankKnownAt = &known
		case "booking":
			effective := f.effectiveAt.UTC()
			r.BookedMatchAt, r.BookingKnownAt = &effective, &known
		}
	}
	return joined, checkpoint, nil
}

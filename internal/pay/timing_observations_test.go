package pay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestTimingObservationSerializedPublicationAndImmutableOriginals(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	db := isolatedPool(t)
	if _, err := db.Exec(ctx, "CREATE SCHEMA pay; "+timingObservationSchema); err != nil {
		t.Fatal(err)
	}
	s := &Service{db: db}
	effective := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	cutoff := effective.Add(3 * time.Hour)
	first := cutoff.Add(time.Minute)
	later := first.Add(time.Hour)
	movement := int64(123)
	p := processorTiming{Operation: "op_1", Payout: "p1", At: effective, timingMovement: timingMovement{Merchant: "mimi", Currency: "USD", Trace: "bank_1", Amount: &movement}}
	source := func(payouts []processorTiming, bank []bankTiming) map[string]*timingRow {
		t.Helper()
		rows, err := timingRows(cutoff, payouts, bank)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	original := source([]processorTiming{p}, nil)
	posted := bankTiming{timingMovement: p.timingMovement, At: effective.Add(time.Minute)}
	complete := source([]processorTiming{p}, []bankTiming{posted})
	fixed := func(at time.Time) func() time.Time { return func() time.Time { return at } }
	if got, err := s.recordTimingObservation(ctx, cutoff, original, fixed(first)); err != nil || !got.Equal(first) {
		t.Fatal("first publication", got, err)
	}

	// Hold a writer after it has validated the complete inventory, before its
	// new observation becomes visible. A replay at this writer's acceptance
	// timestamp must see one complete publication once the writer commits.
	clockReached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	defer resume()
	type stored struct {
		at  time.Time
		err error
	}
	writer := make(chan stored, 1)
	go func() {
		at, err := s.recordTimingObservation(ctx, cutoff, complete, func() time.Time {
			close(clockReached)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return later
		})
		writer <- stored{at, err}
	}()
	select {
	case <-clockReached:
	case <-ctx.Done():
		t.Fatal("writer did not reach publication lock")
	}
	type replay struct {
		row *timingRow
		cp  timingCheckpoint
		err error
	}
	read := make(chan replay, 1)
	go func() {
		rows, cp, err := s.knownTimingRows(ctx, cutoff, later)
		read <- replay{rows["bank_1"], cp, err}
	}()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting bool
		if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_locks WHERE database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND locktype='advisory' AND mode='ShareLock' AND NOT granted)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case got := <-read:
			t.Fatalf("reader skipped publishing writer: %+v", got)
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal("reader never waited for publication")
		}
	}
	resume()
	if got := <-writer; got.err != nil || !got.at.Equal(later) {
		t.Fatal("writer", got)
	}
	got := <-read
	if got.err != nil || !got.cp.KnownAt.Equal(later) || got.row == nil || got.row.Status != "matched" ||
		got.row.BankKnownAt == nil || !got.row.BankKnownAt.Equal(later) || got.row.PayoutKnownAt == nil || !got.row.PayoutKnownAt.Equal(first) {
		t.Fatalf("concurrent replay %+v: %+v", got, got.row)
	}

	// Same-cutoff redelivery does not create another checkpoint, even if
	// concurrent requests arrive with different later receipt times.
	const n = 6
	results := make(chan stored, n)
	for i := range n {
		go func() {
			at, err := s.recordTimingObservation(ctx, cutoff, complete, fixed(later.Add(time.Duration(i+1)*time.Minute)))
			results <- stored{at, err}
		}()
	}
	for range n {
		if r := <-results; r.err != nil || !r.at.Equal(later) {
			t.Fatal("retry changed first publication", r)
		}
	}
	conflict := func(c time.Time, dataset map[string]*timingRow, stamp time.Time) {
		t.Helper()
		_, err := s.recordTimingObservation(ctx, c, dataset, fixed(stamp))
		var f *failure
		if !errors.As(err, &f) || f.code != 409 {
			t.Fatalf("expected conflict at %s: %v", c, err)
		}
	}
	future := later.Add(time.Hour)
	conflict(cutoff.Add(-time.Minute), original, future)
	conflict(cutoff.Add(time.Minute), complete, first)
	conflict(cutoff, original, future)
	mutated := posted
	differentAmount := movement + 1
	mutated.Amount = &differentAmount
	conflict(cutoff, source([]processorTiming{p}, []bankTiming{mutated}), future)

	// Force the fact insert to fail *after* the observation header is written.
	// The other new fact and the header must also roll back.
	invalid := source([]processorTiming{p}, []bankTiming{posted})
	invalid["another-trace"] = &timingRow{Operation: p.Operation, Payout: "another-payout", Trace: "another-trace", Currency: "USD", ProcessorNet: &movement, PayoutAt: &effective}
	conflict(cutoff.Add(time.Minute), invalid, future)
	checkCounts := func(wantObs, wantFacts int) {
		t.Helper()
		var obs, facts int
		if err := db.QueryRow(ctx, "SELECT (SELECT count(*) FROM pay.timing_observations),(SELECT count(*) FROM pay.timing_facts)").Scan(&obs, &facts); err != nil || obs != wantObs || facts != wantFacts {
			t.Fatalf("stored observations and facts (%d,%d) expected (%d,%d): %v", obs, facts, wantObs, wantFacts, err)
		}
	}
	checkCounts(2, 2)
	for _, table := range []string{"timing_observations", "timing_facts"} {
		for _, cmd := range []string{"DELETE FROM pay.%s", "TRUNCATE pay.%s CASCADE"} {
			if _, err := db.Exec(ctx, fmt.Sprintf(cmd, table)); err == nil {
				t.Fatalf("mutable timing table %s", table)
			}
		}
	}
	if _, err := db.Exec(ctx, "UPDATE pay.timing_facts SET amount=amount+1"); err == nil {
		t.Fatal("mutable first-known fact")
	}
	checkCounts(2, 2)
	old, checkpoint, err := s.knownTimingRows(ctx, cutoff, later.Add(-time.Microsecond))
	if err != nil || !checkpoint.KnownAt.Equal(first) || old[p.Trace].Status != "in_transit" || old[p.Trace].BankKnownAt != nil {
		t.Fatal("earlier knowledge changed after later publication", old, checkpoint, err)
	}
	// A successful new interval records full coverage, preserving originals.
	if at, err := s.recordTimingObservation(ctx, cutoff.Add(time.Minute), complete, fixed(future)); err != nil || !at.Equal(future) {
		t.Fatal(at, err)
	}
	checkCounts(3, 2)
}

func TestTimingObservationEmptyInventoryEstablishesCoverage(t *testing.T) {
	ctx := t.Context()
	db := isolatedPool(t)
	if _, err := db.Exec(ctx, "CREATE SCHEMA pay; "+timingObservationSchema); err != nil {
		t.Fatal(err)
	}
	s := &Service{db: db}
	t0 := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	known := t0.Add(time.Minute)
	clock := func() time.Time { return known }
	if _, err := s.recordTimingObservation(ctx, t0, nil, clock); err == nil {
		t.Fatal("missing inventory established coverage")
	}
	if _, _, err := s.knownTimingRows(ctx, t0, known); err == nil {
		t.Fatal("invented empty report before any successful observation")
	}
	if _, err := s.recordTimingObservation(ctx, t0, map[string]*timingRow{}, clock); err != nil {
		t.Fatal(err)
	}
	if rows, cp, err := s.knownTimingRows(ctx, known.Add(time.Hour), known); err != nil || len(rows) != 0 || !cp.EffectiveThrough.Equal(t0) || !cp.KnownAt.Equal(known) {
		t.Fatal("complete empty inventory or coverage boundary lost", rows, cp, err)
	}
	if _, _, err := s.knownTimingRows(ctx, t0, known.Add(-time.Microsecond)); err == nil {
		t.Fatal("future empty observation shown as historical coverage")
	}
}

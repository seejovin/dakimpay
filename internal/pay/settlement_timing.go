package pay

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"
)

// Local booking and source timing are separate observations. A bank credit can
// arrive even if dakim pay rejected or has not imported the processor statement.
type timingRow struct {
	PayoutKnownAt  *time.Time `json:"payout_known_at"`
	BankKnownAt    *time.Time `json:"bank_known_at"`
	BookingKnownAt *time.Time `json:"booking_known_at"`
	Status         string     `json:"status"`
	Trace          string     `json:"bank_trace"`
	Operation      string     `json:"operation_id"`
	Payout         string     `json:"payout_id"`
	Currency       string     `json:"currency"`
	ProcessorNet   *int64     `json:"processor_amount"`
	BankAmount     *int64     `json:"bank_amount"`
	PayoutAt       *time.Time `json:"payout_at"`
	PostedAt       *time.Time `json:"posted_at"`
	AgeSeconds     int64      `json:"age_seconds"`
	BookedMatchAt  *time.Time `json:"booked_match_at"`
}

type timingReport struct {
	KnownAsOf     *time.Time  `json:"known_as_of,omitempty"`
	ObservedAt    *time.Time  `json:"observed_at,omitempty"`
	SourceThrough *time.Time  `json:"source_through,omitempty"`
	Merchant      string      `json:"merchant_id"`
	Currency      string      `json:"currency"`
	AsOf          time.Time   `json:"as_of"`
	Open          []timingRow `json:"open"`
	Matched       []timingRow `json:"matched"`
}

type timingMovement struct {
	Merchant string `json:"merchant_id"`
	Currency string `json:"currency"`
	Trace    string `json:"bank_trace"`
	Amount   *int64 `json:"amount"`
}

type processorTiming struct {
	timingMovement
	Operation string    `json:"operation_id"`
	Payout    string    `json:"payout_id"`
	At        time.Time `json:"created_at"`
}

type bankTiming struct {
	timingMovement
	At time.Time `json:"posted_at"`
}

type timingInventory[T any] struct {
	Merchant string    `json:"merchant_id"`
	Currency string    `json:"currency"`
	AsOf     time.Time `json:"as_of"`
	Complete bool      `json:"complete"`
	Records  []T       `json:"records"`
}

func (v *timingInventory[T]) UnmarshalJSON(raw []byte) error {
	type plain timingInventory[T]
	return decodeBoundedEvidence(raw, (*plain)(v), len(raw))
}

func readTimingInventory[T any](ctx context.Context, s *Service, origin, path string, cutoff time.Time) ([]T, error) {
	var inv timingInventory[T]
	path += "?as_of=" + url.QueryEscape(cutoff.Format(time.RFC3339Nano))
	_, err := s.remote(ctx, origin, http.MethodGet, path, nil, &inv)
	if err != nil || !inv.Complete || inv.Records == nil || inv.Merchant != "mimi" || inv.Currency != "USD" || !inv.AsOf.Equal(cutoff) {
		return nil, invalidTimingEvidence()
	}
	return inv.Records, nil
}

func invalidTimingEvidence() error {
	return fail(502, "A complete, consistent inventory from both the processor and the bank is required. Retry the timing report.")
}

func (m timingMovement) valid(at, cutoff time.Time) bool {
	const maxBrowserInteger = (1 << 53) - 1
	return m.Merchant == "mimi" && m.Currency == "USD" && identifier.MatchString(m.Trace) && m.Amount != nil &&
		*m.Amount >= -maxBrowserInteger && *m.Amount <= maxBrowserInteger && !at.IsZero() && !at.After(cutoff)
}

func timingCutoff(rawQuery string, now time.Time) (time.Time, error) {
	params, err := url.ParseQuery(rawQuery)
	if err != nil {
		return time.Time{}, fail(422, "Invalid timing report query.")
	}
	if len(params) == 0 {
		return now.UTC().Truncate(time.Microsecond), nil
	}
	if len(params) != 1 || len(params["as_of"]) != 1 {
		return time.Time{}, fail(422, "Supply a single as_of timestamp with a time zone.")
	}
	stamp, err := time.Parse(time.RFC3339Nano, params.Get("as_of"))
	if err != nil || stamp.IsZero() || stamp.After(now) {
		return time.Time{}, fail(422, "The report cutoff must be a valid past or present timestamp with a time zone.")
	}
	return stamp.UTC().Truncate(time.Microsecond), nil
}

// Subtract timestamps before rounding down. Unix seconds avoid time.Duration's
// saturation for old history while preserving exact ages over second boundaries.
func wholeSeconds(start, end time.Time) int64 {
	seconds := end.Unix() - start.Unix()
	if end.Nanosecond() < start.Nanosecond() {
		seconds--
	}
	return seconds
}

func timingRows(cutoff time.Time, payouts []processorTiming, bank []bankTiming) (map[string]*timingRow, error) {
	rows := make(map[string]*timingRow, len(payouts))
	operations := make(map[string]bool, len(payouts))
	ids := make(map[string]bool, len(payouts))
	for _, p := range payouts {
		if !p.timingMovement.valid(p.At, cutoff) || !identifier.MatchString(p.Operation) || !identifier.MatchString(p.Payout) ||
			operations[p.Operation] || ids[p.Payout] || rows[p.Trace] != nil {
			return nil, invalidTimingEvidence()
		}
		operations[p.Operation], ids[p.Payout] = true, true
		at := p.At.UTC()
		rows[p.Trace] = &timingRow{Status: "in_transit", Trace: p.Trace, Currency: p.Currency, Operation: p.Operation, Payout: p.Payout, ProcessorNet: p.Amount,
			PayoutAt: &at, AgeSeconds: wholeSeconds(at, cutoff)}
	}
	for _, b := range bank {
		if !b.timingMovement.valid(b.At, cutoff) {
			return nil, invalidTimingEvidence()
		}
		at := b.At.UTC()
		r := rows[b.Trace]
		if r == nil {
			rows[b.Trace] = &timingRow{Status: "bank_only", Trace: b.Trace, Currency: b.Currency, BankAmount: b.Amount, PostedAt: &at, AgeSeconds: wholeSeconds(at, cutoff)}
			continue
		}
		if r.BankAmount != nil {
			return nil, invalidTimingEvidence()
		}
		r.BankAmount, r.PostedAt = b.Amount, &at
		first, last := *r.PayoutAt, at
		if last.Before(first) {
			first, last = last, first
		}
		if *r.ProcessorNet == *r.BankAmount {
			r.Status, r.AgeSeconds = "matched", wholeSeconds(first, last)
		} else {
			r.Status, r.AgeSeconds = "amount_mismatch", wholeSeconds(first, cutoff)
		}
	}
	return rows, nil
}

// Use the timestamp of the durable match event rather than today's payout
// status. Rows without recorded evidence remain unconfirmed in historical views.
func (s *Service) localTimingMatches(ctx context.Context, cutoff time.Time, source map[string]*timingRow) error {
	var traces []string
	for trace, r := range source {
		if r.Status == "matched" {
			traces = append(traces, trace)
		}
	}
	if len(traces) == 0 {
		return nil
	}
	rows, err := s.db.Query(ctx, `SELECT p.operation_id,p.id,p.bank_trace,p.currency,p.net,e.created_at
		FROM pay.payouts p JOIN pay.events e ON e.subject=p.id AND e.kind='matched'
		WHERE p.bank_trace=ANY($1) AND p.status='matched' AND e.created_at>=p.created_at AND e.created_at<=$2
		AND p.bank_evidence=jsonb_build_object('trace',p.bank_trace,'merchant_id','mimi',
			'currency',p.currency,'amount',p.net,'status','posted')`, traces, cutoff)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var op, id, trace, currency string
		var net int64
		var at time.Time
		if err = rows.Scan(&op, &id, &trace, &currency, &net, &at); err != nil {
			return err
		}
		r := source[trace]
		if r.Operation == op && r.Payout == id && r.Currency == currency && *r.BankAmount == net &&
			!at.Before(*r.PayoutAt) && !at.Before(*r.PostedAt) {
			at = at.UTC()
			r.BookedMatchAt = &at
		}
	}
	return rows.Err()
}

func (s *Service) settlementTiming(w http.ResponseWriter, r *http.Request) error {
	cutoff, err := timingCutoff(r.URL.RawQuery, s.timingNow())
	if err != nil {
		return err
	}
	rows, err := s.liveTimingRows(r.Context(), cutoff)
	if err != nil {
		return err
	}
	answer(w, http.StatusOK, assembleTimingReport(cutoff, rows))
	return nil
}

func (s *Service) liveTimingRows(ctx context.Context, cutoff time.Time) (map[string]*timingRow, error) {
	var payouts []processorTiming
	var bank []bankTiming
	var payoutErr, bankErr error
	var reads sync.WaitGroup
	reads.Go(func() {
		payouts, payoutErr = readTimingInventory[processorTiming](ctx, s, s.acquirer, "/sim/settlement-timing", cutoff)
	})
	reads.Go(func() {
		bank, bankErr = readTimingInventory[bankTiming](ctx, s, s.bank, "/bank/settlement-timing", cutoff)
	})
	reads.Wait()
	switch {
	case payoutErr != nil && bankErr != nil:
		return nil, invalidTimingEvidence()
	case payoutErr != nil:
		return nil, fail(502, "The processor payout inventory is incomplete or unavailable at this cutoff. Retry the timing report.")
	case bankErr != nil:
		return nil, fail(502, "The bank posting inventory is incomplete or unavailable at this cutoff. Retry the timing report.")
	}
	union, err := timingRows(cutoff, payouts, bank)
	if err != nil {
		return nil, err
	}
	if err = s.localTimingMatches(ctx, cutoff, union); err != nil {
		return nil, fail(503, "Local booking evidence is unavailable. Retry the timing report.")
	}
	return union, nil
}

func assembleTimingReport(cutoff time.Time, union map[string]*timingRow) timingReport {
	report := timingReport{Merchant: "mimi", Currency: "USD", AsOf: cutoff, Open: []timingRow{}, Matched: []timingRow{}}
	for _, v := range union {
		if v.Status == "matched" {
			report.Matched = append(report.Matched, *v)
		} else {
			report.Open = append(report.Open, *v)
		}
	}
	sort.Slice(report.Open, func(i, j int) bool {
		a, b := report.Open[i], report.Open[j]
		if a.AgeSeconds != b.AgeSeconds {
			return a.AgeSeconds > b.AgeSeconds
		}
		return a.Trace < b.Trace
	})
	matchTime := func(r timingRow) time.Time {
		if r.PayoutAt.After(*r.PostedAt) {
			return *r.PayoutAt
		}
		return *r.PostedAt
	}
	sort.Slice(report.Matched, func(i, j int) bool {
		a, b := matchTime(report.Matched[i]), matchTime(report.Matched[j])
		if !a.Equal(b) {
			return a.After(b)
		}
		return report.Matched[i].Trace < report.Matched[j].Trace
	})
	return report
}

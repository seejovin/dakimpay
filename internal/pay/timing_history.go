package pay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

// Only the local observation service assigns knowledge time. Replaying an
// export, or asking the source-only report for an earlier date, cannot stamp it.
func (s *Service) observeTiming(w http.ResponseWriter, r *http.Request) error {
	if r.URL.RawQuery != "" {
		return fail(422, "Recording an observation uses the current server time. Supply no query parameters.")
	}
	if r.Body != nil {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
		if err != nil {
			return fail(422, "The observation accepts no input data.")
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			var empty map[string]json.RawMessage
			if json.Unmarshal(raw, &empty) != nil || empty == nil || len(empty) != 0 {
				return fail(422, "The observation accepts no input data.")
			}
		}
	}
	ctx := r.Context()
	effective := s.timingNow().UTC().Truncate(time.Microsecond)
	rows, err := s.liveTimingRows(ctx, effective)
	if err != nil {
		return err
	}
	known, err := s.recordTimingObservation(ctx, effective, rows, s.timingNow)
	if err != nil {
		return timingArchiveFailure(err)
	}
	return s.writeTimingHistory(ctx, w, effective, known)
}

func historyCutoffs(raw string, now time.Time) (effective, known time.Time, err error) {
	q, err := url.ParseQuery(raw)
	if err != nil {
		return effective, known, fail(422, "Invalid timing history query.")
	}
	now = now.UTC().Truncate(time.Microsecond)
	if len(q) == 0 {
		return now, now, nil
	}
	for key := range q {
		if key != "as_of" && key != "known_as_of" {
			return effective, known, fail(422, "Unrecognized timing history parameter.")
		}
	}
	if len(q["known_as_of"]) != 1 || len(q["as_of"]) > 1 {
		return effective, known, fail(422, "Supply one known_as_of cutoff and at most one as_of cutoff.")
	}
	parse := func(v string) (time.Time, error) {
		t, e := time.Parse(time.RFC3339Nano, v)
		if e != nil || t.IsZero() {
			return time.Time{}, fail(422, "Enter a UTC or time-zone-qualified cutoff.")
		}
		return t.UTC().Truncate(time.Microsecond), nil
	}
	known, err = parse(q.Get("known_as_of"))
	if err != nil {
		return effective, known, err
	}
	effective = known
	if q.Has("as_of") {
		effective, err = parse(q.Get("as_of"))
		if err != nil {
			return effective, known, err
		}
	}
	if known.After(now) || effective.After(known) {
		return effective, known, fail(422, "Knowledge time must be in the past or present. The effective cutoff must be at or before knowledge time.")
	}
	return effective, known, nil
}

func timingArchiveFailure(err error) error {
	var f *failure
	if errors.As(err, &f) {
		return f
	}
	log.Printf("settlement timing archive: %v", err)
	return fail(503, "Settlement reporting storage is unavailable. Retry the retained report.")
}

func (s *Service) timingHistory(w http.ResponseWriter, r *http.Request) error {
	effective, known, err := historyCutoffs(r.URL.RawQuery, s.timingNow())
	if err != nil {
		return err
	}
	return s.writeTimingHistory(r.Context(), w, effective, known)
}

func (s *Service) writeTimingHistory(ctx context.Context, w http.ResponseWriter, effective, known time.Time) error {
	rows, last, err := s.knownTimingRows(ctx, effective, known)
	if err != nil {
		return timingArchiveFailure(err)
	}
	report := assembleTimingReport(effective, rows)
	report.KnownAsOf = &known
	report.ObservedAt = &last.KnownAt
	report.SourceThrough = &last.EffectiveThrough
	answer(w, http.StatusOK, report)
	return nil
}

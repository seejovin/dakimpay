package pay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"dakimpay/internal/processor"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The current simulated deployment binds one processor to one merchant. The
// source is chosen by the configured endpoint, never by a request header.
const callbackSource = "simulated-acquirer"

func parseEnvelope(raw []byte) (processor.Event, error) {
	var e processor.Event
	d := json.NewDecoder(bytes.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return e, fail(400, "Expected a processor event object.")
	}
	fields := map[string]*string{"event_id": &e.ID, "merchant_id": &e.MerchantID, "operation_id": &e.OperationID, "type": &e.Type}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		dest := fields[name]
		if err != nil || !ok || dest == nil || d.Decode(dest) != nil {
			return e, fail(400, "Invalid or repeated processor event field.")
		}
		delete(fields, name)
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') || d.Decode(new(any)) != io.EOF || len(fields) != 0 {
		return e, fail(400, "Incomplete processor event envelope.")
	}
	if !identifier.MatchString(e.ID) || !identifier.MatchString(e.OperationID) || e.MerchantID != "mimi" {
		return e, fail(422, "Invalid processor event identity or merchant.")
	}
	switch e.Type {
	case "payment.awaiting_customer", "payment.expired", "payment.captured", "refund.succeeded", "refund.observed":
		return e, nil
	default:
		return e, fail(422, "Unsupported processor event type.")
	}
}

// CallbackHandler acknowledges durable receipt. Economic actions never occur in
// this endpoint, and duplicate deliveries do not reschedule work.
func (s *Service) CallbackHandler(signingKey string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /callbacks/processor", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		const limit = 8192
		raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil || len(raw) > limit {
			return fail(413, "Processor event exceeds the request limit.")
		}
		if signingKey == "" || processor.Verify(r.Header, raw, signingKey, time.Now()) != nil {
			return fail(401, "Invalid processor signature or timestamp.")
		}
		e, err := parseEnvelope(raw)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		ctx := r.Context()
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		tag, err := tx.Exec(ctx, `INSERT INTO pay.processor_callbacks(source,event_id,merchant_id,operation_id,event_type,digest)
		 VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, callbackSource, e.ID, e.MerchantID, e.OperationID, e.Type, digest[:])
		if err != nil {
			return err
		}
		accepted := tag.RowsAffected() == 1
		conflict := false
		if !accepted {
			var prior []byte
			err = tx.QueryRow(ctx, "SELECT digest FROM pay.processor_callbacks WHERE source=$1 AND event_id=$2", callbackSource, e.ID).Scan(&prior)
			if err != nil {
				return err
			}
			conflict = !bytes.Equal(prior, digest[:])
			if conflict {
				_, err = tx.Exec(ctx, "INSERT INTO pay.processor_callback_conflicts(source,event_id,digest) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", callbackSource, e.ID, digest[:])
				if err != nil {
					return err
				}
			}
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		if conflict {
			return fail(409, "Event ID was previously accepted with a different payload.")
		}
		status := http.StatusOK
		if accepted {
			status = http.StatusAccepted
		}
		answer(w, status, map[string]string{"event_id": e.ID})
		return nil
	}))
	return mux
}

type Callback struct {
	EventID     string    `json:"event_id"`
	OperationID string    `json:"operation_id"`
	Type        string    `json:"type"`
	Status      string    `json:"status"`
	Detail      string    `json:"detail"`
	Attempts    int       `json:"attempts"`
	Conflicts   int       `json:"conflicts"`
	ReceivedAt  time.Time `json:"received_at"`
}

func callbacks(ctx context.Context, tx pgx.Tx) ([]Callback, error) {
	return list(ctx, tx, `SELECT c.event_id,c.operation_id,c.event_type,c.status,c.detail,c.attempts,
	(SELECT count(*) FROM pay.processor_callback_conflicts x WHERE (x.source,x.event_id)=(c.source,c.event_id)) AS conflicts,c.received_at
	FROM pay.processor_callbacks c WHERE c.merchant_id='mimi'
	ORDER BY (c.status='exception') DESC,conflicts DESC,(c.status='pending') DESC,c.received_at DESC,c.event_id LIMIT 50`, func(row pgx.Row) (Callback, error) {
		var c Callback
		err := row.Scan(&c.EventID, &c.OperationID, &c.Type, &c.Status, &c.Detail, &c.Attempts, &c.Conflicts, &c.ReceivedAt)
		return c, err
	})
}

type callbackJob struct {
	eventID, operation, kind string
	attempt                  int
}

// A claim advances next_attempt before network I/O. A crashed worker is
// re-eligible after its lease; a stale worker cannot finalize a newer attempt.
const claimCallback = `WITH due AS (
 SELECT source,event_id FROM pay.processor_callbacks
 WHERE source=$1 AND status='pending' AND next_attempt<=now()
 ORDER BY next_attempt,event_id LIMIT 1 FOR UPDATE SKIP LOCKED
) UPDATE pay.processor_callbacks c SET attempts=c.attempts+1,
 next_attempt=now()+interval '30 seconds'
 FROM due WHERE (c.source,c.event_id)=(due.source,due.event_id)
 RETURNING c.event_id,c.operation_id,c.event_type,c.attempts`

func (s *Service) ProcessCallbacks(ctx context.Context) error {
	for range 8 {
		var job callbackJob
		err := s.db.QueryRow(ctx, claimCallback, callbackSource).Scan(&job.eventID, &job.operation, &job.kind, &job.attempt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		status, detail, err := s.inspectCallback(ctx, job)
		if err != nil {
			var bad *failure
			var dbErr *pgconn.PgError
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				status, detail = "exception", "No matching local merchant operation."
			case errors.As(err, &bad):
				status, detail = "exception", bad.msg
			case errors.As(err, &dbErr) && dbErr.Code == "23505":
				status, detail = "exception", "The processor reference already belongs to another financial record."
			default:
				return err
			}
		}
		// acceptCharge/acceptRefund already committed the original evidence. If
		// interrupted here, another attempt repeats only a read and a monotonic update.
		_, err = s.db.Exec(ctx, `UPDATE pay.processor_callbacks SET status=$3,detail=$4,checked_at=clock_timestamp(),
		 next_attempt=now()+least(interval '30 minutes',interval '5 seconds' * (1 << least(attempts-1,9)))
		 WHERE source=$1 AND event_id=$2 AND status='pending' AND attempts=$5`, callbackSource, job.eventID, status, detail, job.attempt)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) inspectCallback(ctx context.Context, job callbackJob) (status, detail string, err error) {
	const retry = "Original processor record unavailable. Scheduled for read-only verification."
	if job.kind == "refund.observed" {
		return s.inspectSourceRefund(ctx, job.operation, "")
	}
	if job.kind == "refund.succeeded" {
		f, err := refundByID(ctx, s.db, job.operation)
		if err != nil {
			return "", "", err
		}
		p, err := payment(ctx, s.db, f.PaymentID)
		if err != nil {
			return "", "", err
		}
		var fact refundDisposition
		if _, err = s.remote(ctx, s.acquirer, http.MethodGet, "/sim/refunds/"+url.PathEscape(f.ID), nil, &fact); err != nil {
			return "pending", retry, nil
		}
		if fact.Status != "succeeded" {
			return "exception", "The refund notification conflicts with the original processor outcome.", nil
		}
		if _, err = s.applyRefundOutcome(ctx, f, originalRefund(f, p), fact); err != nil {
			return "", "", err
		}
		return "applied", "Original refund verified against the payment and reserved amount.", nil
	}
	p, err := payment(ctx, s.db, job.operation)
	if err != nil {
		return "", "", err
	}
	if (job.kind == "payment.awaiting_customer" || job.kind == "payment.expired") && p.Channel != "online" {
		return "exception", "Hosted-checkout notification refers to a terminal payment.", nil
	}
	var fact chargeFact
	if _, err = s.remote(ctx, s.acquirer, http.MethodGet, "/sim/charges/"+url.PathEscape(p.ID), nil, &fact); err != nil {
		return "pending", retry, nil
	}
	current, err := s.acceptCharge(ctx, p, fact, true)
	if err != nil {
		return "", "", err
	}
	if fact.Status == "expired" && job.kind == "payment.captured" {
		return "exception", "Capture notification contradicts the acquirer's final unpaid outcome. Review the original operation.", nil
	}
	if current.Status == "expired" && fact.Status != "expired" {
		return "pending", "Processor lookup has not reconfirmed the closed checkout.", nil
	}
	if job.kind == "payment.expired" {
		switch fact.Status {
		case "captured":
			return "applied", "Checkout expiry notification superseded by verified capture.", nil
		case "expired":
			return "applied", "Original checkout verified closed unpaid.", nil
		default:
			return "pending", "Processor lookup has not confirmed closure or capture of the original checkout.", nil
		}
	}
	if fact.Status != "captured" && (job.kind == "payment.captured" || current.Status == "captured") {
		return "pending", "Processor lookup has not confirmed the announced or previously recorded capture.", nil
	}
	if job.kind == "payment.awaiting_customer" && current.Status == "authorized" {
		return "applied", "Earlier checkout notification superseded by verified authorization. Capture remains pending.", nil
	}
	if job.kind == "payment.awaiting_customer" && current.Status == "captured" {
		return "applied", "Earlier checkout notification superseded by verified capture.", nil
	}
	return "applied", "Original payment verified against merchant, order and tender.", nil
}

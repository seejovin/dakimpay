package simulator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"dakimpay/internal/processor"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// enqueueCallback runs inside the financial transaction. Its exact bytes and ID
// survive delivery timeouts, worker crashes and upgrades to the encoder.
func enqueueCallback(ctx context.Context, tx pgx.Tx, merchantID, operationID, kind string) error {
	id, err := tokenID()
	if err != nil {
		return err
	}
	event := processor.Event{ID: "evt_" + id, MerchantID: merchantID, OperationID: operationID, Type: kind}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `insert into sim_callback_outbox(event_id,merchant_id,operation_id,kind,body)
		values($1,$2,$3,$4,$5)`, event.ID, merchantID, operationID, kind, body)
	return err
}

var callbackClient = &http.Client{
	Timeout:       5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// DeliverCallback sends one eligible event to the configured receiver. Attempts
// and acknowledgements are fenced so an expired lease cannot undo a later one.
func DeliverCallback(ctx context.Context, pool *pgxpool.Pool, destination, signingKey string) (bool, error) {
	return deliverCallback(ctx, pool, destination, signingKey, time.Now)
}

func deliverCallback(ctx context.Context, pool *pgxpool.Pool, destination, signingKey string, now func() time.Time) (bool, error) {
	if signingKey == "" || destination == "" {
		return false, errors.New("configure callback destination and signing key")
	}
	var eventID string
	var body []byte
	var attempt int64
	err := pool.QueryRow(ctx, `with due as (
		select event_id from sim_callback_outbox where acknowledged_at is null and next_attempt_at<=now()
		order by next_attempt_at,created_at,event_id limit 1 for update skip locked
	) update sim_callback_outbox o
	set attempts=o.attempts+1,last_attempt_at=now(),next_attempt_at=now()+interval '30 seconds'
	from due where o.event_id=due.event_id returning o.event_id,o.body,o.attempts`).Scan(&eventID, &body, &attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	status, sendErr := sendCallback(ctx, destination, signingKey, eventID, body, now())
	if sendErr == nil {
		_, err = pool.Exec(ctx, `update sim_callback_outbox set acknowledged_at=now(),last_outcome=$3
			where event_id=$1 and attempts=$2 and acknowledged_at is null`, eventID, attempt, fmt.Sprintf("HTTP %d", status))
		return true, err
	}
	outcome := "transport unavailable"
	if status != 0 {
		outcome = fmt.Sprintf("HTTP %d", status)
		if errors.Is(sendErr, errInvalidCallbackReceipt) {
			outcome += ": invalid event receipt"
		}
	}
	// Release the lease after a definite transport result. An interrupted worker
	// that cannot persist this update leaves the original lease to expire.
	_, err = pool.Exec(ctx, `update sim_callback_outbox
		set next_attempt_at=now()+least(interval '5 minutes',interval '2 seconds' * (1 << least(attempts-1,8)::int)),
		    last_outcome=$3 where event_id=$1 and attempts=$2 and acknowledged_at is null`, eventID, attempt, outcome)
	if err != nil {
		return true, err
	}
	return true, fmt.Errorf("callback %s attempt %d: %w", eventID, attempt, sendErr)
}

var errInvalidCallbackReceipt = errors.New("callback receiver did not acknowledge the sent event ID")

func validCallbackReceipt(raw []byte, eventID string) bool {
	// A receipt has one unambiguous event_id. Reject duplicate keys, extra
	// fields and trailing documents even when one of them matches.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	for _, want := range []json.Token{json.Delim('{'), "event_id", eventID, json.Delim('}')} {
		got, err := decoder.Token()
		if err != nil || got != want {
			return false
		}
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func sendCallback(ctx context.Context, destination, key, eventID string, body []byte, now time.Time) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, destination, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	processor.Sign(req.Header, body, key, now)
	res, err := callbackClient.Do(req)
	if err != nil {
		return 0, errors.New("callback response unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusAccepted {
		return res.StatusCode, fmt.Errorf("callback receiver returned HTTP %d", res.StatusCode)
	}
	const maxReceiptBytes = 4096
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxReceiptBytes+1))
	if err != nil || len(raw) > maxReceiptBytes || !validCallbackReceipt(raw, eventID) {
		return res.StatusCode, errInvalidCallbackReceipt
	}
	return res.StatusCode, nil
}

// An abandoned checkout closes at the source even if nobody opens its link or
// polls for status. A competing approval uses the same locked deadline decision.
func expireDueCheckouts(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, `select operation_id from sim_charge where status='awaiting_customer'
		and checkout_expires_at<=statement_timestamp() order by checkout_expires_at,operation_id limit 32`)
	if err != nil {
		return err
	}
	operations, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	a := acquirer{pool: pool}
	for _, op := range operations {
		if _, err = a.lookupCharge(ctx, op, "", false); err != nil {
			return err
		}
	}
	return nil
}

// RunCallbacks drains a bounded number per pass. An unavailable receiver does
// not block a charge, refund, checkout, or a later unrelated callback.
func RunCallbacks(ctx context.Context, pool *pgxpool.Pool, destination, signingKey string) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := expireDueCheckouts(ctx, pool); err != nil && ctx.Err() == nil {
			log.Printf("simulated acquirer checkout expiry: %v", err)
		}
		for range 16 {
			claimed, err := DeliverCallback(ctx, pool, destination, signingKey)
			if err != nil && ctx.Err() == nil {
				log.Printf("simulated acquirer callback: %v", err)
			}
			if !claimed || ctx.Err() != nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"dakimpay/internal/gwstore"
)

// Subscription is provisioned by the local operator. A subscription ID binds its
// endpoint and program. KeyID identifies the current version of its signing key.
type Subscription struct {
	ID       string `json:"id"`
	Merchant string `json:"merchant"`
	Account  string `json:"account"`
	URL      string `json:"url"`
	KeyID    string `json:"key_id"`
	Secret   string `json:"secret"`
}

func ValidateSubscriptions(targets []Subscription) error {
	if len(targets) > 128 {
		return errors.New("more than 128 webhook subscriptions")
	}
	ids := make(map[string]bool)
	keys := make(map[string]bool)
	for _, t := range targets {
		if !engineIdentifier.MatchString(t.ID) || !engineIdentifier.MatchString(t.Merchant) ||
			!engineIdentifier.MatchString(t.Account) || !engineIdentifier.MatchString(t.KeyID) ||
			ids[t.ID] || len(t.Secret) < 32 || len(t.Secret) > 256 || keys[t.Secret] {
			return errors.New("invalid or duplicate webhook subscription or signing secret")
		}
		u, err := url.Parse(t.URL)
		if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil ||
			u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path == "" || u.Port() == "" ||
			u.Host != "127.0.0.1:"+u.Port() || len(t.URL) > 2048 {
			return errors.New("webhook receiver must be an explicit loopback HTTP port and path")
		}
		if port, err := strconv.Atoi(u.Port()); err != nil || port < 1 || port > 65535 {
			return errors.New("invalid webhook port")
		}
		ids[t.ID], keys[t.Secret] = true, true
	}
	return nil
}

type DeliveryStatus struct {
	Subscription string     `json:"subscription_id"`
	Cursor       int64      `json:"cursor"`
	LastAck      string     `json:"last_acknowledged_event,omitempty"`
	LastAckAt    *time.Time `json:"last_acknowledged_at,omitempty"`
	Pending      string     `json:"pending_event,omitempty"`
	Attempts     int64      `json:"pending_attempts"`
	NextAttempt  *time.Time `json:"next_attempt_at,omitempty"`
	LastFailure  string     `json:"last_failure,omitempty"`
}

type deliveryState struct {
	DeliveryStatus
	Merchant       string
	Account        string
	EndpointDigest string
	Sequence       int64
	Payload        []byte
	Lease          string
}

func deliveryKey(id string) string { return engineKey("delivery", id) }

type deliveryEnvelope struct {
	ID       string          `json:"event_id"`
	Version  int             `json:"schema_version"`
	Merchant string          `json:"merchant_id"`
	Event    json.RawMessage `json:"event"`
}

func (s *Service) DeliveryStatus(ctx context.Context, merchant, account, id string) (DeliveryStatus, error) {
	if !engineIdentifier.MatchString(id) {
		return DeliveryStatus{}, engineFail(404, "subscription_not_found")
	}
	var result deliveryState
	err := s.Store.View(ctx, func(tx gwstore.Tx) error {
		found, err := gwstore.GetJSON(tx, deliveryKey(id), &result)
		if err != nil {
			return err
		}
		if !found || result.Merchant != merchant || result.Account != account {
			return engineFail(404, "subscription_not_found")
		}
		return nil
	})
	if err != nil {
		return DeliveryStatus{}, err
	}
	return result.DeliveryStatus, nil
}

func (s *Service) RegisterDelivery(m *http.ServeMux) {
	m.HandleFunc("GET /v2/webhook-subscriptions/{id}", engineMerchant(func(w http.ResponseWriter, r *http.Request, p Principal) error {
		status, err := s.DeliveryStatus(r.Context(), p.Merchant, p.Account, r.PathValue("id"))
		if err != nil {
			return err
		}
		engineSend(w, 200, status)
		return nil
	}))
}

func (s *Service) claimDelivery(ctx context.Context, t Subscription) (deliveryState, error) {
	var job deliveryState
	generation, err := engineID("lease_")
	if err != nil {
		return job, err
	}
	now := s.Now().UTC()
	hash := sha256.Sum256([]byte(t.URL))
	endpoint := hex.EncodeToString(hash[:])
	key := deliveryKey(t.ID)
	err = s.Store.Update(ctx, func(tx gwstore.Tx) error {
		job = deliveryState{}
		var state deliveryState
		found, err := gwstore.GetJSON(tx, key, &state)
		if err != nil {
			return err
		}
		if found && (state.Merchant != t.Merchant || state.Account != t.Account || state.EndpointDigest != endpoint) {
			return errors.New("subscription destination or financial program changed; assign a new subscription ID")
		}
		if !found {
			state = deliveryState{DeliveryStatus: DeliveryStatus{Subscription: t.ID}, Merchant: t.Merchant, Account: t.Account, EndpointDigest: endpoint}
		}
		if state.NextAttempt != nil && now.Before(*state.NextAttempt) {
			return nil
		}
		if state.Pending == "" {
			prefix := engineKey("feed", t.Merchant) + "/"
			rows, err := tx.Scan(prefix, prefix+fmt.Sprintf("%020d", state.Cursor), 32)
			if err != nil {
				return err
			}
			for _, row := range rows {
				var e engineEvent
				if err = json.Unmarshal(row.Value, &e); err != nil {
					return err
				}
				if e.Sequence != state.Cursor+1 || row.Key != prefix+fmt.Sprintf("%020d", e.Sequence) {
					return errors.New("noncontiguous merchant event stream")
				}
				if e.Account != t.Account {
					state.Cursor = e.Sequence
					continue
				}
				// The delivery ID identifies the committed merchant event across subscriptions.
				identity := sha256.Sum256([]byte(row.Key))
				state.Pending = "evt_" + hex.EncodeToString(identity[:])
				state.Sequence = e.Sequence
				state.Payload, err = json.Marshal(deliveryEnvelope{state.Pending, 1, t.Merchant, json.RawMessage(row.Value)})
				if err != nil {
					return err
				}
				if len(state.Payload) > 65536 {
					return errors.New("webhook event exceeds 64 KiB")
				}
				break
			}
			if state.Pending == "" {
				if !found || len(rows) > 0 {
					return gwstore.PutJSON(tx, key, state)
				}
				return nil
			}
		}
		leaseEnd := now.Add(20 * time.Second)
		state.Lease = generation
		state.NextAttempt = &leaseEnd
		if state.Attempts < 1<<62 {
			state.Attempts++
		}
		job = state
		return gwstore.PutJSON(tx, key, state)
	})
	if err != nil {
		return deliveryState{}, err
	}
	return job, nil
}

// An exact acknowledgment has one event_id field containing the expected value,
// and no trailing document. Whitespace is immaterial.
func acknowledges(raw []byte, event string) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	for _, want := range []any{json.Delim('{'), "event_id", event, json.Delim('}')} {
		v, err := d.Token()
		if err != nil || v != want {
			return false
		}
	}
	_, err := d.Token()
	return err == io.EOF
}

func transmitDelivery(ctx context.Context, client *http.Client, t Subscription, job deliveryState, at time.Time) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(job.Payload))
	if err != nil {
		return "invalid_request"
	}
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(t.Secret))
	fmt.Fprintf(mac, "dakim.webhook.v1\n%s\n%s\n%s\n", ts, t.KeyID, t.ID)
	mac.Write(job.Payload)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Dakim-Webhook-Version", "1")
	req.Header.Set("Dakim-Webhook-Timestamp", ts)
	req.Header.Set("Dakim-Webhook-Key-Id", t.KeyID)
	req.Header.Set("Dakim-Webhook-Subscription", t.ID)
	req.Header.Set("Dakim-Webhook-Signature", "v1="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := client.Do(req)
	if err != nil {
		return "transport_error"
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		return fmt.Sprintf("http_%d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2049))
	if err != nil || len(raw) > 2048 || !acknowledges(raw, job.Pending) {
		return "unacknowledged"
	}
	return ""
}

func (s *Service) finishDelivery(ctx context.Context, job deliveryState, failure string) error {
	key := deliveryKey(job.Subscription)
	now := s.Now().UTC()
	return s.Store.Update(ctx, func(tx gwstore.Tx) error {
		var state deliveryState
		found, err := gwstore.GetJSON(tx, key, &state)
		if err != nil {
			return err
		}
		if !found || state.Lease != job.Lease || state.Pending != job.Pending {
			return nil
		}
		state.Lease = ""
		state.LastFailure = failure
		if failure == "" {
			state.Cursor = state.Sequence
			state.LastAck, state.LastAckAt = state.Pending, &now
			state.Pending, state.Payload, state.Attempts, state.NextAttempt = "", nil, 0, nil
		} else {
			delay := time.Second << min(state.Attempts-1, 8)
			jitter := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%d", state.Subscription, state.Pending, state.Attempts)))
			next := now.Add(delay + delay*time.Duration(jitter[0])/1024)
			state.NextAttempt = &next
		}
		return gwstore.PutJSON(tx, key, state)
	})
}

func newDeliveryClient() *http.Client {
	// Never use the host's HTTP proxy for the locally authorized destination.
	return &http.Client{Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4, IdleConnTimeout: 20 * time.Second},
		Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (s *Service) deliverOne(ctx context.Context, client *http.Client, t Subscription) (bool, error) {
	job, err := s.claimDelivery(ctx, t)
	if err != nil || job.Pending == "" {
		return false, err
	}
	failure := transmitDelivery(ctx, client, t, job, s.Now())
	if err := s.finishDelivery(ctx, job, failure); err != nil {
		return false, err
	}
	return failure == "", nil
}

func (s *Service) RunDelivery(ctx context.Context, targets []Subscription) {
	if err := ValidateSubscriptions(targets); err != nil {
		log.Printf("webhook configuration: %v", err)
		return
	}
	client := newDeliveryClient()
	defer client.CloseIdleConnections()
	var workers sync.WaitGroup
	for _, target := range targets {
		workers.Add(1)
		go func(t Subscription) {
			defer workers.Done()
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for ctx.Err() == nil {
				// Drain a bounded slice after each wakeup. A stalled subscription has
				// its own worker and cannot impede processor inquiries or other merchants.
				for range 32 {
					sent, err := s.deliverOne(ctx, client, t)
					if err != nil && ctx.Err() == nil {
						log.Printf("webhook %s polling failed: %s", t.ID, strings.TrimSpace(err.Error()))
					}
					if !sent || err != nil {
						break
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
			}
		}(target)
	}
	workers.Wait()
}

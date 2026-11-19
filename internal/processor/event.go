// Package processor defines the local simulator's signed notification contract.
// External processors may authenticate a different envelope at the connector boundary.
package processor

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"
)

type Event struct {
	ID          string `json:"event_id"`
	MerchantID  string `json:"merchant_id"`
	OperationID string `json:"operation_id"`
	Type        string `json:"type"`
}

const (
	timestampHeader = "Dakim-Event-Timestamp"
	signatureHeader = "Dakim-Event-Signature"
	tolerance       = 5 * time.Minute
)

func digest(raw []byte, key, timestamp string) []byte {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("dakim.callback.v1." + timestamp + "."))
	mac.Write(raw)
	return mac.Sum(nil)
}

// Sign timestamps each delivery attempt. The event identifier and raw body remain stable on retry.
func Sign(headers http.Header, raw []byte, key string, now time.Time) {
	timestamp := strconv.FormatInt(now.Unix(), 10)
	headers.Set(timestampHeader, timestamp)
	headers.Set(signatureHeader, "v1="+hex.EncodeToString(digest(raw, key, timestamp)))
}

func Verify(headers http.Header, raw []byte, key string, now time.Time) error {
	invalid := errors.New("invalid processor callback signature")
	if key == "" || len(headers.Values(timestampHeader)) != 1 || len(headers.Values(signatureHeader)) != 1 {
		return invalid
	}
	ts, sig := headers.Get(timestampHeader), headers.Get(signatureHeader)
	seconds, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || strconv.FormatInt(seconds, 10) != ts || seconds < now.Add(-tolerance).Unix() || seconds > now.Add(tolerance).Unix() {
		return invalid
	}
	if len(sig) != len("v1=")+2*sha256.Size || sig[:3] != "v1=" {
		return invalid
	}
	provided, err := hex.DecodeString(sig[3:])
	if err != nil || !hmac.Equal(provided, digest(raw, key, ts)) {
		return invalid
	}
	return nil
}

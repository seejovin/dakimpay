package processor

import (
	"net/http"
	"testing"
	"time"
)

func TestSignedDeliveryContract(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	const key = "synthetic-signing-fixture"
	raw := []byte(`{"event_id":"e1"}`)
	h := http.Header{}
	Sign(h, raw, key, now)
	// Independently computed HMAC-SHA256 vector for the documented byte sequence.
	if h.Get(signatureHeader) != "v1=4d503392d5771ba3b5efa4a1cf4a399d37ff3aeb539dbe24b1b7ecf0ced23a6b" || Verify(h, raw, key, now) != nil {
		t.Fatal("wire contract changed")
	}
	for _, offset := range []time.Duration{-301 * time.Second, 301 * time.Second} {
		if Verify(h, raw, key, now.Add(offset)) == nil {
			t.Fatal("out-of-window attempt accepted")
		}
	}
	if Verify(h, append(append([]byte{}, raw...), '\n'), key, now) == nil || Verify(h, raw, "wrong-key", now) == nil {
		t.Fatal("unbound callback")
	}
	h.Add(signatureHeader, h.Get(signatureHeader))
	if Verify(h, raw, key, now) == nil {
		t.Fatal("ambiguous signature accepted")
	}
}

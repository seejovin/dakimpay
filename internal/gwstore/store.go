// Package gwstore provides a strongly consistent, serializable record store.
// Update callbacks can run several times. They must not perform network effects
// outside the database or publish results until Update returns successfully.
package gwstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	MaxKeyBytes   = 1024
	MaxValueBytes = 1 << 20
	MaxScan       = 1000
)

var ErrReadOnly = errors.New("cannot mutate a read-only transaction")

type Store interface {
	View(ctx context.Context, fn func(Tx) error) error
	Update(ctx context.Context, fn func(Tx) error) error
}

type Tx interface {
	Get(key string) (value []byte, found bool, err error)
	Put(key string, value []byte) error
	Delete(key string) error
	// Scan returns at most limit entries, in bytewise key order, whose keys
	// start with prefix and are strictly greater than after. Empty after starts
	// at the beginning of the prefix. Limits must be within 1..MaxScan.
	Scan(prefix, after string, limit int) ([]Entry, error)
}

type Entry struct {
	Key   string
	Value []byte
}

func GetJSON(tx Tx, key string, target any) (bool, error) {
	data, found, err := tx.Get(key)
	if err != nil || !found {
		return false, err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return false, fmt.Errorf("decode stored record: %w", err)
	}
	return true, nil
}

func PutJSON(tx Tx, key string, record any) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return tx.Put(key, data)
}

func checkKey(k string) error {
	if len(k) == 0 || len(k) > MaxKeyBytes {
		return errors.New("record key length outside supported range")
	}
	return nil
}

func checkWrite(k string, v []byte) error {
	if err := checkKey(k); err != nil {
		return err
	}
	if len(v) > MaxValueBytes {
		return errors.New("record value exceeds size limit")
	}
	return nil
}

func checkScan(prefix, after string, limit int) error {
	if len(prefix) > MaxKeyBytes || len(after) > MaxKeyBytes || limit < 1 || limit > MaxScan {
		return errors.New("invalid record scan bounds")
	}
	return nil
}

// prefixEnd is the first key above the entire prefix. An empty result indicates
// no finite upper bound, as with an empty or all-0xff prefix.
func prefixEnd(prefix string) []byte {
	end := []byte(prefix)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 255 {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

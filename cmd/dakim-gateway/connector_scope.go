package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"

	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
)

// Bind the record store to one role, even when two DSNs name the same database.
func claimStore(ctx context.Context, store gwstore.Store, role string) error {
	const key = "system/database-purpose-v1"
	return store.Update(ctx, func(tx gwstore.Tx) error {
		old, ok, err := tx.Get(key)
		if err != nil {
			return err
		}
		if ok {
			if string(old) != role {
				return errors.New("database already belongs to a different service")
			}
			return nil
		}
		// Earlier releases have gateway records but no role marker.
		if role != "merchant-gateway" {
			rows, err := tx.Scan("gw2/", "", 1)
			if err != nil {
				return err
			}
			if len(rows) > 0 {
				return errors.New("partner database contains existing gateway history")
			}
		}
		return tx.Put(key, []byte(role))
	})
}

type nativeOwner struct{ Merchant, Account, Connector, Route string }

// A local connector ID can be renamed in configuration. Upstream account
// ownership must survive both a rename and replacement of the adapter database.
func claimPartner(ctx context.Context, store gwstore.Store, kind string, sourceIDs []string, owner nativeOwner) error {
	return store.Update(ctx, func(tx gwstore.Tx) error {
		for i, id := range sourceIDs {
			if id == "" {
				return errors.New("missing native account identity")
			}
			key := fmt.Sprintf("system/partner-account/%s/%d/%x", kind, i, sha256.Sum256([]byte(id)))
			var old nativeOwner
			seen, err := gwstore.GetJSON(tx, key, &old)
			if err != nil {
				return err
			}
			if seen && old != owner {
				return errors.New("native partner identity already belongs to another enrollment")
			}
			if !seen {
				if err := gwstore.PutJSON(tx, key, owner); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

type fullProcessor interface {
	railcontract.Processor
	railcontract.ProcessorIdentity
	railcontract.ReleaseInspector
}

// Validate files and database ownership on start. Verify the external test
// account on first use, so a disconnected partner cannot stop other routes.
type lazyPartner struct {
	mu      sync.Mutex
	ready   fullProcessor
	loading chan struct{}
	nextTry time.Time
	open    func(context.Context) (fullProcessor, error)
}

func (l *lazyPartner) client(ctx context.Context) (fullProcessor, error) {
	for {
		l.mu.Lock()
		if l.ready != nil {
			client := l.ready
			l.mu.Unlock()
			return client, nil
		}
		if wake := l.loading; wake != nil {
			l.mu.Unlock()
			select {
			case <-wake:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if time.Now().Before(l.nextTry) {
			l.mu.Unlock()
			return nil, errors.New("partner test account unavailable")
		}
		wake := make(chan struct{})
		l.loading = wake
		l.mu.Unlock()
		client, err := l.open(ctx)
		l.mu.Lock()
		if err == nil {
			l.ready = client
		} else {
			l.nextTry = time.Now().Add(2 * time.Second)
		}
		l.loading = nil
		close(wake)
		l.mu.Unlock()
		return client, err
	}
}
func (l *lazyPartner) Identity(ctx context.Context, c railcontract.Command) (string, error) {
	p, e := l.client(ctx)
	if e != nil {
		return "", e
	}
	return p.Identity(ctx, c)
}
func (l *lazyPartner) Submit(ctx context.Context, c railcontract.Command) error {
	p, e := l.client(ctx)
	if e != nil {
		return e
	}
	return p.Submit(ctx, c)
}
func (l *lazyPartner) Lookup(ctx context.Context, c railcontract.Command) (railcontract.Fact, bool, error) {
	p, e := l.client(ctx)
	if e != nil {
		return railcontract.Fact{}, false, e
	}
	return p.Lookup(ctx, c)
}
func (l *lazyPartner) InspectRelease(ctx context.Context, c railcontract.Command) (railcontract.ReleaseInventory, error) {
	p, e := l.client(ctx)
	if e != nil {
		return railcontract.ReleaseInventory{}, e
	}
	return p.InspectRelease(ctx, c)
}

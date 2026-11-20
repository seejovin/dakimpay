package gwstore

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	"cloud.google.com/go/spanner"
	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/api/iterator"
)

// SpannerDDL is applied by the migration principal, not by the runtime.
//
//go:embed spanner.sql
var SpannerDDL string

type spannerStore struct{ client *spanner.Client }

type spannerReader interface {
	Query(context.Context, spanner.Statement) *spanner.RowIterator
}
type spannerTx struct {
	ctx    context.Context
	reader spannerReader
	writer *spanner.ReadWriteTransaction
}

func NewSpanner(ctx context.Context, client *spanner.Client) (Store, error) {
	if client == nil {
		return nil, errors.New("Spanner client is required")
	}
	// Probe the actual relation and both columns using the runtime identity.
	it := client.Single().Query(ctx, spanner.Statement{SQL: "SELECT RecordKey,RecordValue FROM GatewayRecords LIMIT 1"})
	defer it.Stop()
	if _, err := it.Next(); err != nil && err != iterator.Done {
		return nil, fmt.Errorf("open gateway table: %w", err)
	}
	return &spannerStore{client}, nil
}

func (s *spannerStore) View(ctx context.Context, fn func(Tx) error) error {
	tx := s.client.ReadOnlyTransaction()
	defer tx.Close()
	return fn(spannerTx{ctx: ctx, reader: tx})
}

func (s *spannerStore) Update(ctx context.Context, fn func(Tx) error) error {
	// The SDK retries aborted transactions. Pin serializable isolation and
	// only current read-write transactions (no asynchronous commit or stale reads).
	_, err := s.client.ReadWriteTransactionWithOptions(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		return fn(spannerTx{ctx: ctx, reader: tx, writer: tx})
	}, spanner.TransactionOptions{IsolationLevel: spannerpb.TransactionOptions_SERIALIZABLE})
	return err
}

func (t spannerTx) Get(key string) ([]byte, bool, error) {
	if err := checkKey(key); err != nil {
		return nil, false, err
	}
	it := t.reader.Query(t.ctx, spanner.Statement{SQL: "SELECT RecordValue FROM GatewayRecords WHERE RecordKey=@k", Params: map[string]any{"k": []byte(key)}})
	defer it.Stop()
	row, err := it.Next()
	if err == iterator.Done {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var value []byte
	if err = row.Column(0, &value); err != nil {
		return nil, false, err
	}
	return value, true, nil
}

func (t spannerTx) Put(key string, value []byte) error {
	if t.writer == nil {
		return ErrReadOnly
	}
	if err := checkWrite(key, value); err != nil {
		return err
	}
	if value == nil {
		value = []byte{}
	}
	// DML is visible to later reads in this transaction. Buffered mutations
	// would be applied only at commit and could hide reservations from Scan.
	_, err := t.writer.Update(t.ctx, spanner.Statement{
		SQL:    "INSERT OR UPDATE INTO GatewayRecords (RecordKey,RecordValue) VALUES (@k,@v)",
		Params: map[string]any{"k": []byte(key), "v": value},
	})
	return err
}

func (t spannerTx) Delete(key string) error {
	if t.writer == nil {
		return ErrReadOnly
	}
	if err := checkKey(key); err != nil {
		return err
	}
	_, err := t.writer.Update(t.ctx, spanner.Statement{
		SQL:    "DELETE FROM GatewayRecords WHERE RecordKey=@k",
		Params: map[string]any{"k": []byte(key)},
	})
	return err
}

func (t spannerTx) Scan(prefix, after string, limit int) ([]Entry, error) {
	if err := checkScan(prefix, after, limit); err != nil {
		return nil, err
	}
	sql := "SELECT RecordKey,RecordValue FROM GatewayRecords WHERE RecordKey >= @prefix AND RecordKey > @after"
	params := map[string]any{"prefix": []byte(prefix), "after": []byte(after), "limit": int64(limit)}
	if upper := prefixEnd(prefix); upper != nil {
		sql += " AND RecordKey < @upper"
		params["upper"] = upper
	}
	it := t.reader.Query(t.ctx, spanner.Statement{SQL: sql + " ORDER BY RecordKey LIMIT @limit", Params: params})
	defer it.Stop()
	var entries []Entry
	for {
		row, err := it.Next()
		if err == iterator.Done {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
		var key, value []byte
		if err := row.Columns(&key, &value); err != nil {
			return nil, err
		}
		entries = append(entries, Entry{string(key), value})
	}
}

package gwstore

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed postgres.sql
var PostgresDDL string

// InstallPostgres requires migration privileges. Runtime service roles need
// schema USAGE and SELECT, INSERT, UPDATE and DELETE on gateway.records.
func InstallPostgres(ctx context.Context, pool *pgxpool.Pool) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// Concurrent initializers can race on PostgreSQL catalog uniqueness
		// even when both use CREATE ... IF NOT EXISTS.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(924651702)"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, PostgresDDL)
		return err
	})
}

type pgStore struct{ pool *pgxpool.Pool }
type pgTx struct {
	ctx      context.Context
	tx       pgx.Tx
	readOnly bool
}

func NewPostgres(ctx context.Context, pool *pgxpool.Pool) (Store, error) {
	if pool == nil {
		return nil, errors.New("database pool is required")
	}
	_, err := pool.Exec(ctx, "SELECT record_key,record_value FROM gateway.records LIMIT 0")
	if err != nil {
		return nil, fmt.Errorf("open gateway table: %w", err)
	}
	return &pgStore{pool}, nil
}

func (s *pgStore) View(ctx context.Context, fn func(Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly, DeferrableMode: pgx.Deferrable}, func(tx pgx.Tx) error {
		return fn(pgTx{ctx, tx, true})
	})
}

func (s *pgStore) Update(ctx context.Context, fn func(Tx) error) error {
	// Retry only errors for which PostgreSQL definitely aborted the transaction.
	// Connection loss at COMMIT has an unknown outcome; it must propagate.
	for attempt := 0; ; attempt++ {
		err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
			return fn(pgTx{ctx, tx, false})
		})
		var dbErr *pgconn.PgError
		if attempt >= 15 || !errors.As(err, &dbErr) || (dbErr.Code != "40001" && dbErr.Code != "40P01") {
			return err
		}
		wait := time.Duration(1+rand.IntN(1<<min(attempt+1, 7))) * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (t pgTx) Get(key string) ([]byte, bool, error) {
	if err := checkKey(key); err != nil {
		return nil, false, err
	}
	var value []byte
	err := t.tx.QueryRow(t.ctx, "SELECT record_value FROM gateway.records WHERE record_key=$1", []byte(key)).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return value, err == nil, err
}
func (t pgTx) Put(key string, value []byte) error {
	if t.readOnly {
		return ErrReadOnly
	}
	if err := checkWrite(key, value); err != nil {
		return err
	}
	if value == nil {
		value = []byte{}
	}
	_, err := t.tx.Exec(t.ctx, "INSERT INTO gateway.records(record_key,record_value) VALUES($1,$2) ON CONFLICT(record_key) DO UPDATE SET record_value=EXCLUDED.record_value", []byte(key), value)
	return err
}
func (t pgTx) Delete(key string) error {
	if t.readOnly {
		return ErrReadOnly
	}
	if err := checkKey(key); err != nil {
		return err
	}
	_, err := t.tx.Exec(t.ctx, "DELETE FROM gateway.records WHERE record_key=$1", []byte(key))
	return err
}
func (t pgTx) Scan(prefix, after string, limit int) ([]Entry, error) {
	if err := checkScan(prefix, after, limit); err != nil {
		return nil, err
	}
	upper := prefixEnd(prefix)
	query := "SELECT record_key,record_value FROM gateway.records WHERE record_key >= $1 AND record_key > $2"
	args := []any{[]byte(prefix), []byte(after), limit}
	if upper != nil {
		query += " AND record_key < $4"
		args = append(args, upper)
	}
	rows, err := t.tx.Query(t.ctx, query+" ORDER BY record_key LIMIT $3", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		var key, value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		entries = append(entries, Entry{string(key), value})
	}
	return entries, rows.Err()
}

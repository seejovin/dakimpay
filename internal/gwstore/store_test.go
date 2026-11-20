package gwstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/spanner"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStorePostgres(t *testing.T) {
	dsn := os.Getenv("DAKIM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable DAKIM_TEST_DATABASE_URL")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := InstallPostgres(ctx, pool); err != nil {
		t.Fatal(err)
	}
	s, err := NewPostgres(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	qualify(t, s)
}

func TestStoreSpanner(t *testing.T) {
	name := os.Getenv("DAKIM_TEST_SPANNER_DATABASE")
	if name == "" || os.Getenv("SPANNER_EMULATOR_HOST") == "" {
		t.Skip("requires disposable Spanner emulator and DAKIM_TEST_SPANNER_DATABASE")
	}
	ctx := t.Context()
	client, err := spanner.NewClient(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	s, err := NewSpanner(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	qualify(t, s)
}

func qualify(t *testing.T, s Store) {
	t.Helper()
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		t.Fatal(err)
	}
	base := "qualify/" + hex.EncodeToString(salt[:]) + "/"
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	t.Run("transaction_visibility_and_pagination", func(t *testing.T) {
		space := base + "keys/"
		err := s.Update(ctx, func(tx Tx) error {
			for _, id := range []string{"z", "\xff", "a", "abc"} {
				if err := tx.Put(space+id, []byte(id)); err != nil {
					return err
				}
			}
			if err := tx.Put(base+"neighbor", nil); err != nil {
				return err
			}
			if err := tx.Put(space+"a", nil); err != nil {
				return err
			}
			v, found, err := tx.Get(space + "a")
			if err != nil {
				return err
			}
			if !found || len(v) != 0 {
				return fmt.Errorf("empty overwrite lost: found %v", found)
			}
			rows, err := tx.Scan(space, "", 2)
			if err != nil {
				return err
			}
			if len(rows) != 2 || rows[0].Key != space+"a" || rows[1].Key != space+"abc" {
				return fmt.Errorf("first page: %+v", rows)
			}
			next, err := tx.Scan(space, rows[1].Key, 2)
			if err != nil {
				return err
			}
			if len(next) != 2 || next[0].Key != space+"z" || !bytes.Equal(next[1].Value, []byte{255}) {
				return fmt.Errorf("second page: %+v", next)
			}
			if err := tx.Delete(space + "z"); err != nil {
				return err
			}
			if _, ok, err := tx.Get(space + "z"); err != nil {
				return err
			} else if ok {
				return errors.New("deleted key visible")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.View(ctx, func(tx Tx) error {
			rows, err := tx.Scan(space, "", 5)
			if err != nil {
				return err
			}
			if len(rows) != 3 {
				return fmt.Errorf("committed records: %+v", rows)
			}
			if !errors.Is(tx.Put(space+"a", nil), ErrReadOnly) || !errors.Is(tx.Delete(space+"a"), ErrReadOnly) {
				return errors.New("view accepted write")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("abort_reverts_all_records", func(t *testing.T) {
		stopped := errors.New("cancel command")
		key := base + "rolled-back"
		err := s.Update(ctx, func(tx Tx) error {
			if err := tx.Put(key, []byte("control")); err != nil {
				return err
			}
			if err := tx.Put(key+"/outbox", nil); err != nil {
				return err
			}
			return stopped
		})
		if !errors.Is(err, stopped) {
			t.Fatalf("got %v, want command failure", err)
		}
		if err := s.View(ctx, func(tx Tx) error {
			rows, err := tx.Scan(key, "", 5)
			if len(rows) != 0 {
				return errors.New("aborted command or outbox committed")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("serializable_range_reservation", func(t *testing.T) {
		// Every competitor sees the same capacity and may choose a different key.
		// Snapshot isolation alone can allow every one to reserve the last slot.
		const workers = 8
		prefix := base + "slot/"
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				errs <- s.Update(ctx, func(tx Tx) error {
					rows, err := tx.Scan(prefix, "", 1)
					if err != nil || len(rows) > 0 {
						return err
					}
					return tx.Put(fmt.Sprintf("%s%d", prefix, i), []byte("reserved"))
				})
			}(i)
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := s.View(ctx, func(tx Tx) error {
			rows, err := tx.Scan(prefix, "", workers)
			if err == nil && len(rows) != 1 {
				return fmt.Errorf("got %d reservations for one slot", len(rows))
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
}

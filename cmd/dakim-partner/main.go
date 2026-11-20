// Command dakim-partner inspects and recovers original Stripe sandbox
// operations. The merchant gateway retains authority to originate payments.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"dakimpay/internal/partnerstripe"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: dakim-partner check|inspect|recover -config PATH [-operation ID] [-object ID]")
	}
	flags := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
	path := flags.String("config", "", "private Stripe connector config file")
	op := flags.String("operation", "", "original gateway operation ID")
	object := flags.String("object", "", "original Stripe Checkout Session or Refund ID")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	mode := os.Args[1]
	if flags.NArg() != 0 || *path == "" || (mode != "check" && mode != "inspect" && mode != "recover") || (mode == "check" && (*op != "" || *object != "")) || (mode != "check" && *op == "") || (mode == "recover") != (*object != "") {
		return errors.New("invalid arguments for check, inspect or recover")
	}
	f, err := os.OpenFile(*path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("cannot open private connector file")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 65536 {
		return errors.New("connector config must be a private regular file of at most 64 KiB")
	}
	var cfg struct {
		DatabaseURL string               `json:"database_url"`
		Settings    partnerstripe.Config `json:"settings"`
	}
	dec := json.NewDecoder(io.LimitReader(f, 65537))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&cfg); err != nil || dec.Decode(new(any)) != io.EOF {
		return errors.New("invalid connector config")
	}
	if err = cfg.Settings.Validate(); err != nil {
		return err
	}
	pg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil || cfg.DatabaseURL == "" {
		return errors.New("invalid database_url")
	}
	local := func(host string) bool { return filepath.IsAbs(host) || net.ParseIP(host).IsLoopback() }
	if !local(pg.ConnConfig.Host) {
		return errors.New("dedicated local database required")
	}
	for _, v := range pg.ConnConfig.Fallbacks {
		if !local(v.Host) {
			return errors.New("database fallback must be local")
		}
	}
	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, end := context.WithTimeout(stop, 35*time.Second)
	defer end()
	pool, err := pgxpool.NewWithConfig(ctx, pg)
	if err != nil {
		return err
	}
	defer pool.Close()
	a, err := partnerstripe.New(ctx, pool, cfg.Settings)
	if err != nil {
		return err
	}
	if mode == "check" {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"mode": "test", "account_id": cfg.Settings.AccountID, "status": "account_verified"})
	}
	var ev partnerstripe.Evidence
	if mode == "recover" {
		ev, err = a.Recover(ctx, *op, *object)
	} else {
		ev, err = a.Read(ctx, *op)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(ev)
}

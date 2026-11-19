// Command simulate-source-refund records a refund inside the local acquirer
// simulator. There is no HTTP money-moving endpoint for merchant or buyer use.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"dakimpay/internal/simulator"
	"github.com/jackc/pgx/v5/pgxpool"
)

func run() error {
	state := flag.String("state", "", "private local simulator state directory")
	port := flag.Int("port", 0, "local simulator PostgreSQL socket port")
	operation := flag.String("operation", "", "stable synthetic refund operation ID starting with sr_")
	capture := flag.String("capture", "", "original acquirer capture reference")
	amount := flag.Int64("amount", 0, "positive amount in USD cents")
	flag.Parse()
	if *state == "" || *port < 1 || *port > 65535 || *operation == "" || *capture == "" || *amount < 1 || flag.NArg() != 0 {
		return errors.New("supply -state, -port, -operation, -capture and -amount in cents")
	}
	dir, err := filepath.Abs(*state)
	if err != nil {
		return err
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("a private simulator directory is required")
	}
	// Connect only to the acquirer's local socket and existing simulator role.
	// A supplied database URL could accidentally select another installation.
	q := url.Values{
		"host": {filepath.Join(dir, "run")}, "port": {strconv.Itoa(*port)},
		"user": {"dakim_acquirer"}, "sslmode": {"disable"}, "passfile": {os.DevNull},
	}
	dsn := url.URL{Scheme: "postgresql", Path: "/dakim_acquirer", RawQuery: q.Encode()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := pgxpool.New(ctx, dsn.String())
	if err != nil {
		return err
	}
	defer db.Close()
	result, err := simulator.RecordSourceRefund(ctx, db, *operation, *capture, "mimi", "USD", *amount)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

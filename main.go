package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dakimpay/internal/console"
	"dakimpay/internal/pay"
	"dakimpay/internal/simulator"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed web
var assets embed.FS

func env(name string) string {
	v := os.Getenv("DAKIM_" + name)
	if v == "" {
		log.Fatal("Missing DAKIM_" + name)
	}
	return v
}

func origin(name string) string {
	v := env(name)
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Port() == "" {
		log.Fatal(name + " must be an http://127.0.0.1:port origin")
	}
	return v
}

func run(ctx context.Context, role string) error {
	pool, err := pgxpool.New(ctx, env("DB"))
	if err != nil {
		return err
	}
	defer pool.Close()
	public := origin("ORIGIN")
	addr, _ := url.Parse(public)
	var h http.Handler
	var worker func(context.Context)
	switch role {
	case "bank":
		h, err = simulator.BankWithKeys(ctx, pool, env("SERVICE_TOKEN"), env("INSTRUCTION_TOKEN"), env("POST_TOKEN"))
	case "acquirer":
		h, err = simulator.AcquirerWithKeys(ctx, pool, public, origin("BANK_URL"), env("SERVICE_TOKEN"), env("INSTRUCTION_TOKEN"), env("APPROVE_TOKEN"))
	case "pay":
		bank, acq := origin("BANK_URL"), origin("ACQUIRER_URL")
		service, e := pay.New(ctx, pool, acq, bank, env("SERVICE_TOKEN"))
		if e != nil {
			return e
		}
		web, e := fs.Sub(assets, "web")
		if e != nil {
			return e
		}
		worker = service.RunRecovery
		h = console.New(public, acq, bank, env("MERCHANT_TOKEN"), env("SESSION_SECRET"), env("APPROVE_TOKEN"), env("POST_TOKEN"), service.Handler(), web)
	default:
		return errors.New("usage: dakimpay pay|acquirer|bank")
	}
	if err != nil {
		return err
	}
	guard := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != addr.Host {
			http.Error(w, "Unrecognized local host", 421)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		if r.URL.Path == "/health" && r.Method == "GET" {
			probe, cancel := context.WithTimeout(r.Context(), time.Second)
			defer cancel()
			if err := pool.Ping(probe); err != nil {
				http.Error(w, "Database unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"ok":true,"service":%q,"mode":"simulation"}`, role)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		h.ServeHTTP(w, r)
	})
	server := &http.Server{Addr: addr.Host, Handler: guard, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 60 * time.Second}
	l, err := net.Listen("tcp", addr.Host)
	if err != nil {
		return err
	}
	if worker != nil {
		work, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); worker(work) }()
		defer func() { cancel(); <-done }()
	}
	go func() {
		<-ctx.Done()
		t, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = server.Shutdown(t)
	}()
	log.Printf("%s simulation listening on %s", role, public)
	if err = server.Serve(l); errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: dakimpay pay|acquirer|bank")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1]); err != nil {
		log.Fatal(err)
	}
}

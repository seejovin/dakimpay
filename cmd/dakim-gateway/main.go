// Command dakim-gateway operates the independent gateway and its virtual processor.
// All public listeners in this build are restricted to loopback for qualification.
package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/spanner"
	"dakimpay/internal/aggregation"
	"dakimpay/internal/gateway"
	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
	"dakimpay/internal/virtualrail"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed web
var web embed.FS

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: dakim-gateway gateway|virtual-acquirer|migrate")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1]); err != nil {
		log.Fatal(err)
	}
}

var databaseName = regexp.MustCompile(`^projects/[a-zA-Z0-9_-]+/instances/[a-zA-Z0-9_-]+/databases/[a-zA-Z0-9_-]+$`)

func localPostgres(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid local PostgreSQL configuration")
	}
	local := func(host string) bool { return filepath.IsAbs(host) || net.ParseIP(host).IsLoopback() }
	if !local(cfg.ConnConfig.Host) {
		return nil, errors.New("qualification PostgreSQL must use a loopback IP or a local socket")
	}
	for _, f := range cfg.ConnConfig.Fallbacks {
		if !local(f.Host) {
			return nil, errors.New("qualification PostgreSQL fallback must be local")
		}
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

func storage(ctx context.Context, migrate bool) (gwstore.Store, func(), error) {
	if name := os.Getenv("DAKIM_SPANNER_DATABASE"); name != "" {
		if !databaseName.MatchString(name) || !emulatorEndpoint(os.Getenv("SPANNER_EMULATOR_HOST")) {
			return nil, nil, errors.New("qualification build requires an explicit database and loopback Spanner emulator")
		}
		c, err := spanner.NewClient(ctx, name)
		if err != nil {
			return nil, nil, err
		}
		store, err := gwstore.NewSpanner(ctx, c)
		if err != nil {
			c.Close()
			return nil, nil, err
		}
		return store, c.Close, nil
	}
	dsn := os.Getenv("DAKIM_DB")
	if dsn == "" {
		return nil, nil, errors.New("missing DAKIM_DB")
	}
	pool, err := localPostgres(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	if migrate {
		if err := gwstore.InstallPostgres(ctx, pool); err != nil {
			pool.Close()
			return nil, nil, err
		}
	}
	store, err := gwstore.NewPostgres(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	return store, pool.Close, nil
}

func run(ctx context.Context, role string) error {
	if role == "migrate" {
		_, close, err := storage(ctx, true)
		if err == nil {
			close()
		}
		return err
	}
	var origin string
	var app http.Handler
	var probe func(context.Context) error
	var worker func(context.Context)
	switch role {
	case "gateway":
		var cfg gatewayConfig
		if err := readConfig(os.Getenv("DAKIM_GATEWAY_CONFIG"), &cfg); err != nil {
			return err
		}
		principal, err := cfg.principals()
		if err != nil {
			return err
		}
		store, closeStore, err := storage(ctx, false)
		if err != nil {
			return err
		}
		defer closeStore()
		if err := claimStore(ctx, store, "merchant-gateway"); err != nil {
			return err
		}
		rail, catalog, closeConnectors, err := cfg.openConnectors(ctx, store)
		if err != nil {
			return err
		}
		defer closeConnectors()
		core := gateway.New(store, rail)
		if catalog != nil {
			core.SelectRoute = catalog.Select
			core.RouteExists = catalog.RouteExists
			core.RouteCandidates = catalog.RouteCandidates
		}
		allowed := make(map[string]gateway.Principal)
		for _, p := range principal {
			if p.Role == gateway.MerchantRole {
				allowed[p.Merchant] = p
			}
		}
		core.CurrentProgram = func(merchant string) (gateway.Principal, bool) {
			p, ok := allowed[merchant]
			return p, ok
		}
		core.CanDispatch = func(c railcontract.Command) bool {
			p, ok := allowed[c.Merchant]
			if !ok || c.Account != p.Account || c.Currency != p.Currency || !p.Outlets[c.Outlet] || (catalog != nil && !catalog.CanDispatch(c)) {
				return false
			}
			if c.Kind == "refund" {
				return true
			}
			return c.Amount > 0 && c.Amount <= p.MaxAmount && (c.Channel != "terminal" || p.Terminals[c.Terminal] == c.Outlet)
		}
		app = registerHTTP(core, principal, catalog, cfg)
		worker = func(ctx context.Context) {
			done := make(chan struct{})
			go func() { defer close(done); core.RunDelivery(ctx, cfg.Subscriptions) }()
			core.RunWorker(ctx)
			<-done
		}
		origin = cfg.Origin
		probe = func(ctx context.Context) error {
			return store.View(ctx, func(tx gwstore.Tx) error { _, _, err := tx.Get("system/readiness"); return err })
		}
	case "virtual-acquirer":
		var cfg railConfig
		if err := readConfig(os.Getenv("DAKIM_GATEWAY_CONFIG"), &cfg); err != nil {
			return err
		}
		progs, err := cfg.programs()
		if err != nil {
			return err
		}
		dsn := os.Getenv("DAKIM_DB")
		if dsn == "" {
			return errors.New("missing virtual rail database")
		}
		pool, err := localPostgres(ctx, dsn)
		if err != nil {
			return err
		}
		defer pool.Close()
		app, err = virtualrail.New(ctx, pool, cfg.Origin, cfg.RailToken, progs)
		if err != nil {
			return err
		}
		origin = cfg.Origin
		probe = pool.Ping
	default:
		return errors.New("unknown process role")
	}
	u, err := loopbackOrigin(origin)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: guard(u.Host, origin, role, probe, app), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	l, err := net.Listen("tcp", u.Host)
	if err != nil {
		return err
	}
	work, cancel := context.WithCancel(ctx)
	if worker != nil {
		done := make(chan struct{})
		go func() { defer close(done); worker(work) }()
		defer func() { cancel(); <-done }()
	} else {
		defer cancel()
	}
	go func() {
		<-work.Done()
		shutdown, end := context.WithTimeout(context.Background(), 10*time.Second)
		defer end()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("%s listening on %s (virtual qualification)", role, origin)
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func guard(host, origin, role string, probe func(context.Context) error, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		if r.Host != host {
			http.Error(w, "unrecognized host", 421)
			return
		}
		if values := r.Header.Values("Origin"); len(values) > 1 || (len(values) == 1 && values[0] != origin) {
			http.Error(w, "origin not permitted", 403)
			return
		}
		if len(r.Header.Values("Authorization")) > 1 {
			http.Error(w, "ambiguous authorization", 401)
			return
		}
		if r.URL.Path == "/health" && r.Method == http.MethodGet {
			ctx, end := context.WithTimeout(r.Context(), 2*time.Second)
			defer end()
			if err := probe(ctx); err != nil {
				http.Error(w, "database unavailable", 503)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"ok":true,"service":%q,"mode":"virtual"}`, role)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
		next.ServeHTTP(w, r)
	})
}

func gatewayHTTP(core *gateway.Service, principals map[[32]byte]gateway.Principal) http.Handler {
	return registerHTTP(core, principals, nil, gatewayConfig{})
}

func registerHTTP(core *gateway.Service, principals map[[32]byte]gateway.Principal, catalog *aggregation.Router, cfg gatewayConfig) http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /v2/payment-methods", func(w http.ResponseWriter, r *http.Request) { paymentMethods(w, r, catalog, core) })
	api.HandleFunc("GET /ui/profile", func(w http.ResponseWriter, r *http.Request) {
		p, _ := gateway.PrincipalFrom(r.Context())
		if p.Role != gateway.MerchantRole {
			http.Error(w, "merchant credential required", 403)
			return
		}
		// Scope recovery data on shared registers to the authenticated operator and current grant.
		grant, err := gateway.GrantFingerprint(p)
		if err != nil {
			http.Error(w, "invalid merchant profile", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"checkout_origins": cfg.checkoutOrigins(p), "aggregate": catalog != nil, "methods": map[string][]string{"terminal": methodsFor(catalog, p, "terminal", 0), "hosted": methodsFor(catalog, p, "hosted", 0)}, "storage_scope": grant, "merchant": p.Merchant, "outlets": p.Outlets, "terminals": p.Terminals, "currency": p.Currency, "max_amount": p.MaxAmount})
	})
	api.Handle("/", core.Handler())
	contents, _ := fs.Sub(web, "web")
	assets := http.FileServer(http.FS(contents))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; object-src 'none'; base-uri 'none'")
		switch r.URL.Path {
		case "/", "/app.css", "/app.js":
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "method not allowed", 405)
				return
			}
			assets.ServeHTTP(w, r)
			return
		}
		s := r.Header.Get("Authorization")
		raw, ok := strings.CutPrefix(s, "Bearer ")
		if !ok || len(raw) < 32 || len(raw) > 256 {
			http.Error(w, "valid credential required", 401)
			return
		}
		p, ok := principals[sha256.Sum256([]byte(raw))]
		if !ok {
			http.Error(w, "valid credential required", 401)
			return
		}
		api.ServeHTTP(w, r.WithContext(gateway.WithPrincipal(r.Context(), p)))
	})
}

package pay

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

type Service struct {
	db                    *pgxpool.Pool
	acquirer, bank, token string
	client                *http.Client
}

func New(ctx context.Context, pool *pgxpool.Pool, acquirerURL, bankURL, serviceToken string) (*Service, error) {
	if pool == nil || serviceToken == "" {
		return nil, errors.New("database and service authentication required")
	}
	for _, raw := range []string{acquirerURL, bankURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("invalid simulator origin")
		}
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		return nil, err
	}
	return &Service{db: pool, acquirer: strings.TrimRight(acquirerURL, "/"), bank: strings.TrimRight(bankURL, "/"), token: serviceToken, client: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

type failure struct {
	code int
	msg  string
}

func (f *failure) Error() string      { return f.msg }
func fail(code int, msg string) error { return &failure{code, msg} }
func reject(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return fail(409, "Operation conflicts with an existing financial record.")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fail(404, "Record not found.")
	}
	return err
}
func answer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type action func(http.ResponseWriter, *http.Request) error

func endpoint(fn action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			err = reject(err)
			var f *failure
			if errors.As(err, &f) {
				answer(w, f.code, map[string]string{"error": f.msg})
				return
			}
			log.Printf("payment service: %v", err)
			answer(w, 500, map[string]string{"error": "Payment records could not be updated. Retry the original operation."})
		}
	}
}
func (s *Service) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/dashboard", endpoint(s.dashboard))
	m.HandleFunc("POST /v1/payments", endpoint(s.createPayment))
	m.HandleFunc("GET /v1/payments/{id}", endpoint(s.getPayment))
	m.HandleFunc("POST /v1/payments/{id}/recover", endpoint(s.recover))
	m.HandleFunc("POST /v1/payments/{id}/refunds", endpoint(s.refund))
	m.HandleFunc("GET /v1/refunds/{id}", endpoint(s.getRefund))
	m.HandleFunc("POST /v1/refunds/{id}/recover", endpoint(s.recoverRefund))
	m.HandleFunc("POST /v1/settle", endpoint(s.settle))
	m.HandleFunc("GET /v1/payouts/{id}/statement", endpoint(s.merchantStatement))
	m.HandleFunc("POST /v1/reconcile", endpoint(s.reconcile))
	return m
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func key(r *http.Request) (string, error) {
	k := r.Header.Get("Idempotency-Key")
	if !identifier.MatchString(k) {
		return "", fail(400, "Supply a valid Idempotency-Key header.")
	}
	return k, nil
}
func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 16385))
	if err != nil || len(b) > 16384 {
		return fail(400, "Invalid request body.")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(&struct{}{}) != io.EOF {
		return fail(400, "Supply one valid JSON object with supported fields.")
	}
	return nil
}
func newID(prefix string) string {
	var id [16]byte
	_, err := rand.Read(id[:])
	if err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(id[:])
}
func (s *Service) remote(ctx context.Context, base, method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("simulator response %d", resp.StatusCode)
	}
	d := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
	if err = d.Decode(out); err != nil {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

type sqlDB interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func note(ctx context.Context, tx pgx.Tx, subject, kind, msg string) error {
	_, err := tx.Exec(ctx, "INSERT INTO pay.events(subject,kind,message) VALUES ($1,$2,$3) ON CONFLICT (subject,kind) DO NOTHING", subject, kind, msg)
	return err
}

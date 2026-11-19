package simulator

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const merchant = "mimi"
const currency = "USD"
const limit = 100_000_000 // minor units, per simulated operation

type failure struct {
	status  int
	message string
}

func (f failure) Error() string { return f.message }
func invalid(msg string) error  { return failure{http.StatusUnprocessableEntity, msg} }
func conflict(msg string) error { return failure{http.StatusConflict, msg} }

func jsonResponse(w http.ResponseWriter, status int, object any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(object)
}

func respondError(w http.ResponseWriter, err error) {
	var f failure
	switch {
	case errors.As(err, &f):
		jsonResponse(w, f.status, map[string]string{"error": f.message})
	case errors.Is(err, pgx.ErrNoRows):
		jsonResponse(w, 404, map[string]string{"error": "Record not found"})
	default:
		jsonResponse(w, 500, map[string]string{"error": "Simulator could not complete the operation"})
	}
}

func require(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			jsonResponse(w, 401, map[string]string{"error": "Service credential required"})
			return
		}
		next(w, r)
	}
}

func readJSON(r *http.Request, object any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32769))
	if err != nil || len(raw) > 32768 {
		return failure{400, "Invalid or oversized request"}
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(object); err != nil {
		return failure{400, "Invalid request fields"}
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return failure{400, "Expected one JSON object"}
	}
	return nil
}

func validID(s string) bool {
	return len(s) > 0 && len(s) <= 200 && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n\t")
}

func tokenID() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes[:]), nil
}

func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// All writes for the single training merchant serialize at this boundary. A
// payout can therefore never see half a capture/refund. Network I/O follows commit.
func write(ctx context.Context, p *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, p, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(641889301)"); err != nil {
			return err
		}
		return fn(tx)
	})
}
func health(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			jsonResponse(w, 503, map[string]bool{"ok": false})
			return
		}
		jsonResponse(w, 200, map[string]bool{"ok": true})
	}
}
func money(cents int64) string {
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}

package console

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"
)

type console struct {
	origin, acquirer, bank, merchantToken, session, approveToken, postToken string
	api                                                                     http.Handler
	assets                                                                  http.Handler
	client                                                                  *http.Client
}

func New(origin, acquirer, bank, merchantToken, session, approveToken, postToken string, api http.Handler, web fs.FS) http.Handler {
	return &console{origin, acquirer, bank, merchantToken, session, approveToken, postToken, api, http.FileServer(http.FS(web)), &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func equal(a, b string) bool {
	return len(a) > 0 && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, msg string) {
	respond(w, status, map[string]string{"error": msg})
}
func (c *console) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if !strings.HasPrefix(r.URL.Path, "/v1/") && !strings.HasPrefix(r.URL.Path, "/app/") {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			http.SetCookie(w, &http.Cookie{Name: "dakim_console", Value: c.session, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		}
		c.assets.ServeHTTP(w, r)
		return
	}
	origin := r.Header.Get("Origin")
	if origin != "" && origin != c.origin {
		fail(w, 403, "Unexpected request origin")
		return
	}
	cookie, e := r.Cookie("dakim_console")
	local := e == nil && equal(cookie.Value, c.session)
	machine := equal(r.Header.Get("Authorization"), "Bearer "+c.merchantToken)
	if (!local && !machine) || (r.Header.Get("Sec-Fetch-Site") == "cross-site") {
		fail(w, 401, "Open the local console or use a merchant credential")
		return
	}
	if !machine && r.Method != "GET" && origin != c.origin {
		fail(w, 403, "A same-origin browser request is required")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		if machine && (r.URL.Path == "/v1/settle" || r.URL.Path == "/v1/reconcile") {
			fail(w, 403, "Simulation operator session required")
			return
		}
		c.api.ServeHTTP(w, r)
		return
	}
	if !local || origin != c.origin || r.Method != "POST" {
		fail(w, 403, "Simulation operator session required")
		return
	}
	switch {
	case r.URL.Path == "/app/demo":
		report, err := c.demo(r.Context())
		if err != nil {
			fail(w, 409, err.Error())
			return
		}
		respond(w, 200, report)
	case strings.HasPrefix(r.URL.Path, "/app/bank-post/"):
		trace := strings.TrimPrefix(r.URL.Path, "/app/bank-post/")
		if trace == "" || strings.Contains(trace, "/") {
			fail(w, 400, "Invalid bank trace")
			return
		}
		if err := c.external(r.Context(), c.bank+"/bank/post/"+url.PathEscape(trace), c.postToken); err != nil {
			fail(w, 409, err.Error())
			return
		}
		respond(w, 200, map[string]bool{"posted": true})
	default:
		fail(w, 404, "Unknown simulation action")
	}
}

func (c *console) external(ctx context.Context, path, token string) error {
	req, e := http.NewRequestWithContext(ctx, "POST", path, nil)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, e := c.client.Do(req)
	if e != nil {
		return errors.New("Simulator unavailable. Retry the original operation.")
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("Simulator returned HTTP %d", res.StatusCode)
	}
	return nil
}

func (c *console) call(ctx context.Context, method, path, key string, payload any, out any) error {
	var body bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&body).Encode(payload); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, path, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	// The guided merchant uses the same API handler, including durable idempotency and provider transport.
	res := httptest.NewRecorder()
	c.api.ServeHTTP(res, req)
	if res.Code/100 != 2 {
		return fmt.Errorf("%s %s (HTTP %d): %s", method, path, res.Code, strings.TrimSpace(res.Body.String()))
	}
	if out != nil {
		return json.Unmarshal(res.Body.Bytes(), out)
	}
	return nil
}

type payment struct{ ID, Status string }
type payout struct {
	ID, Status string
	Net        int
	BankTrace  string `json:"bank_trace"`
}

func (c *console) demo(ctx context.Context) (map[string]any, error) {
	method := "POST"
	create := func(order, channel, brand string, amount int, lost bool) (payment, error) {
		in := map[string]any{"order_id": order, "channel": channel, "brand": brand, "currency": "USD", "amount": amount, "lose_response": lost}
		if channel == "terminal" {
			in["terminal_id"] = "MIMI-01"
		}
		var p payment
		err := c.call(ctx, method, "/v1/payments", "guide-create-"+channel, in, &p)
		if err == nil && p.ID == "" {
			err = errors.New("Missing original payment ID")
		}
		return p, err
	}
	recover := func(p payment) error {
		var got payment
		if err := c.call(ctx, method, "/v1/payments/"+url.PathEscape(p.ID)+"/recover", "", nil, &got); err != nil {
			return err
		}
		if got.ID != p.ID || got.Status != "captured" {
			return errors.New("Original capture is still unconfirmed")
		}
		return nil
	}
	t, err := create("MIMI-GUIDED-TERMINAL", "terminal", "visa", 4200, true)
	if err != nil {
		return nil, err
	}
	if t.Status != "captured" {
		if err = recover(t); err != nil {
			return nil, err
		}
	}
	o, err := create("MIMI-GUIDED-ONLINE", "online", "mastercard", 6800, false)
	if err != nil {
		return nil, err
	}
	if o.Status != "captured" {
		if err = c.external(ctx, c.acquirer+"/sim/approve/"+url.PathEscape(o.ID), c.approveToken); err != nil {
			return nil, err
		}
		if err = recover(o); err != nil {
			return nil, err
		}
	}
	var refund payment
	if err = c.call(ctx, method, "/v1/payments/"+url.PathEscape(o.ID)+"/refunds", "guide-refund-online", map[string]int{"amount": 1000}, &refund); err != nil {
		return nil, err
	}
	if refund.Status != "succeeded" {
		if err = c.call(ctx, method, "/v1/refunds/"+url.PathEscape(refund.ID)+"/recover", "", nil, &refund); err != nil {
			return nil, err
		}
		if refund.Status != "succeeded" {
			return nil, errors.New("Original refund is still unconfirmed")
		}
	}
	var p payout
	if err = c.call(ctx, method, "/v1/settle", "guide-payout", map[string]string{"batch": "guided"}, &p); err != nil {
		return nil, err
	}
	if p.Net != 9746 || p.BankTrace == "" {
		return nil, errors.New("Guided statement differs from the expected $97.46. Investigate before posting.")
	}
	if p.Status != "matched" {
		if err = c.external(ctx, c.bank+"/bank/post/"+url.PathEscape(p.BankTrace), c.postToken); err != nil {
			return nil, err
		}
		if err = c.call(ctx, method, "/v1/reconcile", "", nil, nil); err != nil {
			return nil, err
		}
		if err = c.call(ctx, method, "/v1/settle", "guide-payout", map[string]string{"batch": "guided"}, &p); err != nil {
			return nil, err
		}
	}
	if p.Status != "matched" {
		return nil, errors.New("Bank entry has not reconciled")
	}
	return map[string]any{"terminal_payment": t.ID, "online_payment": o.ID, "refund": refund.ID, "payout": p.ID, "bank_trace": p.BankTrace, "deposited": p.Net, "currency": "USD", "status": p.Status}, nil
}

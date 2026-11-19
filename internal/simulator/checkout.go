package simulator

import (
	"html/template"
	"net/http"
	"net/url"
)

var checkoutPage = template.Must(template.New("checkout").Funcs(template.FuncMap{
	"money": money,
	"brand": func(s string) string {
		return map[string]string{"visa": "Visa", "mastercard": "Mastercard", "amex": "American Express"}[s]
	},
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Mimi · Acquirer checkout</title>
<style>
:root{color-scheme:light;font-family:ui-sans-serif,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;color:#192e2b;background:#f2f1ec}
*{box-sizing:border-box}body{margin:0;min-height:100svh;display:grid;place-items:center;padding:22px}
main{width:min(440px,100%);background:#fcfbf7;border:1px solid #c5cec5;border-radius:14px;box-shadow:0 12px 50px #15342a13;overflow:hidden}
header{background:#15382f;color:#fff8e7;display:flex;justify-content:space-between;align-items:center;padding:22px 26px}
header strong{font-family:Georgia,serif;font-size:25px;font-weight:normal}header span{font-size:12px;color:#e6d7ae;border:1px solid #72897b;padding:5px 8px;border-radius:20px}
section{padding:25px}h1{font:normal 31px/1.15 Georgia,serif;margin:0 0 18px}p{font-size:14px;line-height:1.55;color:#4a5a55;margin:8px 0}
dl{margin:20px 0}dl>div{display:flex;justify-content:space-between;gap:14px;padding:11px 0;border-bottom:1px solid #dedfd3;font-size:14px}
dt{color:#596660}dd{margin:0;text-align:right;overflow-wrap:anywhere}.total dd{font-weight:650;font-size:20px}
button{width:100%;color:#fff;background:#194b3b;border:0;border-radius:7px;padding:14px;font-size:15px;font-weight:600;cursor:pointer}button:focus-visible{outline:3px solid #b17921;outline-offset:3px}
</style></head>
<body><main><header><strong>Mimi</strong><span>Simulated acquirer</span></header><section>
{{if eq .Status "captured"}}<h1>Payment approved</h1><p>Return to dakim pay and check the payment status. The acquirer has recorded this simulated capture.</p>{{else if eq .Status "expired"}}<h1>Checkout expired</h1><p>The acquirer closed this checkout unpaid. Return to Mimi for help with this order.</p>{{else}}<h1>Approve your payment</h1><p>This hosted checkout uses a test instrument. Select approve to simulate the issuer’s decision.</p>{{end}}
<dl><div><dt>Merchant</dt><dd>Mimi</dd></div><div><dt>Order</dt><dd>{{.Order}}</dd></div><div><dt>Test instrument</dt><dd>{{brand .Brand}}</dd></div><div class="total"><dt>Total · USD</dt><dd>{{money .Amount}}</dd></div></dl>
{{if eq .Status "awaiting_customer"}}<form method="post"><button type="submit">Approve simulated payment</button></form>{{else}}<p>Acquirer reference</p><p style="overflow-wrap:anywhere">{{.Reference}}</p>{{end}}
</section></main></body></html>`))

func (a *acquirer) checkout(w http.ResponseWriter, r *http.Request) {
	var c charge
	var err error
	if r.Method == http.MethodPost {
		if source := r.Header.Get("Origin"); source != "" {
			u, parseErr := url.Parse(source)
			expected, _ := url.Parse(a.publicURL)
			if parseErr != nil || u.Scheme != expected.Scheme || u.Host != expected.Host {
				respondError(w, failure{403, "Checkout origin mismatch"})
				return
			}
		}
		c, err = a.lookupCharge(r.Context(), "", r.PathValue("capability"), true)
	} else {
		c, err = a.lookupCharge(r.Context(), "", r.PathValue("capability"), false)
	}
	if err != nil {
		respondError(w, err)
		return
	}
	// Chromium strips Origin on an HTML form POST under no-referrer. Retain
	// the same-origin form's provenance while omitting external referrers.
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if c.Status == "expired" {
		w.WriteHeader(http.StatusGone)
	}
	_ = checkoutPage.Execute(w, c)
}

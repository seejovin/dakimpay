package virtualrail

import (
	"embed"
	"fmt"
	"html/template"
	"mime"
	"net/http"
	"strconv"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/text/currency"
)

//go:embed static/*
var web embed.FS

var buyer = template.Must(template.ParseFS(web, "static/checkout.html"))

func amountLabel(code string, minor int64) string {
	unit, err := currency.ParseISO(code)
	if err != nil {
		return fmt.Sprintf("%d minor units (%s)", minor, code)
	}
	scale, _ := currency.Standard.Rounding(unit)
	digits := strconv.FormatInt(minor, 10)
	if scale > 0 {
		if len(digits) <= scale {
			digits = strings.Repeat("0", scale+1-len(digits)) + digits
		}
		digits = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	}
	return code + " " + digits
}
func (s *service) registerUI(m *http.ServeMux) {
	for path, asset := range map[string]string{"/terminal": "reader.html", "/terminal.js": "reader.js", "/terminal.css": "terminal.css"} {
		file, err := web.ReadFile("static/" + asset)
		if err != nil {
			panic(err)
		}
		typ := "text/html; charset=utf-8"
		switch asset {
		case "reader.js":
			typ = "application/javascript; charset=utf-8"
		case "terminal.css":
			typ = "text/css; charset=utf-8"
		}
		m.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", typ)
			_, _ = w.Write(file)
		})
	}
	m.HandleFunc("GET /checkout/{capability}", s.buyer)
	m.HandleFunc("POST /checkout/{capability}", s.buyer)
	m.HandleFunc("GET /wallet/{capability}", s.buyer)
	m.HandleFunc("POST /wallet/{capability}", s.buyer)
	m.HandleFunc("GET /checkout/{capability}/qr.png", s.walletQR)
}
func (s *service) buyer(w http.ResponseWriter, r *http.Request) {
	// Browsers redact Origin on a form POST from a no-referrer document.
	// Send only the origin so the checkout capability never enters Referer.
	w.Header().Set("Referrer-Policy", "strict-origin")
	decision, instrument := "", ""
	if r.Method == http.MethodPost {
		if r.Header.Get("Origin") != s.origin {
			writeFailure(w, reject(403, "Origin required for customer action."))
			return
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/x-www-form-urlencoded" {
			writeFailure(w, reject(415, "Expected a checkout form."))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 2048)
		if r.ParseForm() != nil || len(r.PostForm["decision"]) != 1 || len(r.PostForm["card_brand"]) > 1 || len(r.PostForm) != (1+len(r.PostForm["card_brand"])) {
			writeFailure(w, reject(400, "Invalid customer decision."))
			return
		}
		decision = r.PostForm.Get("decision")
		instrument = r.PostForm.Get("card_brand")
		if decision != "approve" && decision != "decline" {
			writeFailure(w, reject(422, "Choose approve or decline."))
			return
		}
	}
	capability := r.PathValue("capability")
	walletScreen := strings.Contains(r.Pattern, "/wallet/")
	f, err := s.hosted(r.Context(), capability, decision, instrument, walletScreen)
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	label := map[string]string{"card": "Card", "visa": "Visa", "mastercard": "Mastercard", "amex": "American Express", "wechat_pay": "WeChat Pay", "alipay": "Alipay"}
	_ = buyer.Execute(w, struct {
		Merchant, Order, Amount, State, Reference, Method, MethodName, Brand, WalletURL, QRURL string
		WalletScreen, Wallet                                                                   bool
	}{f.Merchant, f.Order, amountLabel(f.Currency, f.Amount), f.State, f.Reference, f.PaymentMethod, label[f.PaymentMethod], label[f.CardBrand], "/wallet/" + capability, "/checkout/" + capability + "/qr.png", walletScreen, wallet(f.PaymentMethod)})
}

// QR data is the virtual wallet's local handoff URL. Real wallet apps cannot
// authorize these synthetic presentations. Rendering is read-only and expiry
// is checked against the original processor record on every request.
func (s *service) walletQR(w http.ResponseWriter, r *http.Request) {
	capability := r.PathValue("capability")
	f, err := s.hosted(r.Context(), capability, "", "", true)
	if err != nil {
		writeFailure(w, err)
		return
	}
	if f.PaymentMethod != "wechat_pay" || f.State != "pending" {
		writeFailure(w, reject(404, "No pending virtual QR presentation."))
		return
	}
	body, err := qrcode.Encode(s.origin+"/wallet/"+capability, qrcode.Medium, 240)
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(body)
}

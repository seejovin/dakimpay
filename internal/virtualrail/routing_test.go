package virtualrail

import (
	"bytes"
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dakimpay/internal/railcontract"
)

func TestRoutedPresentationsAndSource(t *testing.T) {
	for _, tc := range []struct{ method, brand string }{{"visa", "visa"}, {"mastercard", "mastercard"}, {"amex", "amex"}, {"card", "mastercard"}, {"wechat_pay", ""}, {"alipay", ""}} {
		t.Run(tc.method, func(t *testing.T) {
			ctx := t.Context()
			db := fixtureDB(t)
			p := Program{RouteID: "route-" + tc.method, ConnectorID: "provider-" + tc.method, ProviderAccount: "sub-mimi", PaymentMethods: []string{tc.method},
				Merchant: "mimi", Account: "program", Currency: "USD", Beneficiary: "bank", StatementToken: "test-only-statement-long", Outlets: []string{"grocery"}, MaxAmount: 10000}
			srv := httptest.NewUnstartedServer(nil)
			origin := "http://" + srv.Listener.Addr().String()
			h, err := New(ctx, db, origin, testConnector, []Program{p})
			if err != nil {
				t.Fatal(err)
			}
			srv.Config.Handler = h
			srv.Start()
			t.Cleanup(srv.Close)
			c, err := NewClient(origin, testConnector)
			if err != nil {
				t.Fatal(err)
			}
			cmd := railcontract.Command{Merchant: p.Merchant, Account: p.Account, RouteID: p.RouteID, ConnectorID: p.ConnectorID, ProviderAccount: p.ProviderAccount, PaymentMethod: tc.method,
				Kind: "charge", ID: "same-order", Order: "basket", Outlet: "grocery", Channel: "hosted", Amount: 970, Currency: "USD"}
			id, err := c.Identity(ctx, cmd)
			if err != nil {
				t.Fatal(err)
			}
			ctx = railcontract.WithProcessorIdentity(ctx, id)
			wrong := cmd
			wrong.ProviderAccount = "another-enrollment"
			if _, err = c.Identity(ctx, wrong); err == nil {
				t.Fatal("foreign acquiring account accepted")
			}
			if _, _, err = c.Lookup(ctx, wrong); err == nil {
				t.Fatal("authoritative absence for a foreign acquiring account")
			}
			if err = c.Submit(ctx, wrong); err == nil {
				t.Fatal("processor accepted foreign route binding")
			}
			if err = c.Submit(ctx, cmd); err != nil {
				t.Fatal(err)
			}
			f, found, err := c.Lookup(ctx, cmd)
			if err != nil || !found || f.State != "pending" {
				t.Fatalf("presentation not pending: %v %v", f, err)
			}
			wire := func(method, url, body, token string, status int) []byte {
				t.Helper()
				req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Origin", origin)
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				resp, err := srv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				raw, err := io.ReadAll(resp.Body)
				if err != nil || resp.StatusCode != status {
					t.Fatalf("%s request: status %d, expected %d (%s, %v)", method, resp.StatusCode, status, raw, err)
				}
				return raw
			}
			buyer := f.CheckoutURL
			if wallet(tc.method) {
				page := wire("GET", buyer, "", "", 200)
				if !bytes.Contains(page, []byte("Open simulated")) {
					t.Fatal("wallet handoff not offered")
				}
				buyer = strings.Replace(buyer, "/checkout/", "/wallet/", 1)
				wire("POST", f.CheckoutURL, "decision=approve", "", 404)
				if tc.method == "wechat_pay" {
					img, err := png.Decode(bytes.NewReader(wire("GET", f.CheckoutURL+"/qr.png", "", "", 200)))
					if err != nil || img.Bounds().Dx() != 240 {
						t.Fatal("invalid QR presentation", err)
					}
				} else {
					wire("GET", f.CheckoutURL+"/qr.png", "", "", 404)
				}
				wire("POST", buyer, "decision=approve&card_brand=visa", "", 422)
			} else if tc.method != "card" {
				other := "visa"
				if tc.method == "visa" {
					other = "amex"
				}
				wire("POST", buyer, "decision=approve&card_brand="+other, "", 422)
			}
			form := "decision=approve"
			if tc.brand != "" {
				form += "&card_brand=" + tc.brand
			}
			wire("POST", buyer, form, "", 200)
			wire("POST", buyer, form, "", 200)
			if tc.method == "wechat_pay" {
				wire("GET", f.CheckoutURL+"/qr.png", "", "", 404)
			}
			payment, found, err := c.Lookup(ctx, cmd)
			if err != nil || !found || payment.State != "captured" || payment.CardBrand != tc.brand || payment.Command != cmd {
				t.Fatalf("capture mismatch: %+v %v", payment, err)
			}
			if tc.method == "card" {
				wire("POST", buyer, "decision=approve&card_brand=amex", "", 409)
			}
			parent := cmd
			refund := parent
			refund.Amount = 70
			refund.ID = "returned"
			refund.Kind = "refund"
			refund.Parent = parent.ID
			for i := 0; i < 2; i++ {
				if err = c.Submit(ctx, refund); err != nil {
					t.Fatal(err)
				}
			}
			inventory, err := c.InspectRelease(ctx, parent)
			if err != nil || !inventory.Complete || len(inventory.Refunds) != 1 || inventory.Refunds[0].Command != refund || inventory.Refunds[0].CardBrand != tc.brand {
				t.Fatalf("inventory %+v %v", inventory, err)
			}
			url := origin + "/rail/v1/remittances/daily-001"
			var report settlementReport
			raw := wire("POST", url, "", p.StatementToken, 201)
			if json.Unmarshal(raw, &report) != nil || report.RouteID != p.RouteID || report.ProviderAccount != p.ProviderAccount || report.Net != 876 {
				t.Fatalf("native report: %s", raw)
			}
			if !bytes.Equal(raw, wire("POST", url, "", p.StatementToken, 200)) {
				t.Fatal("frozen statement changed")
			}
			var src nativeRemittance
			if err = json.Unmarshal(wire("GET", url+"/source", "", p.StatementToken, 200), &src); err != nil {
				t.Fatal(err)
			}
			if src.Ledger != id || src.ConnectorID != p.ConnectorID || src.RouteID != p.RouteID || src.ProviderAccount != p.ProviderAccount || len(src.Movements) != 2 {
				t.Fatalf("native source: %+v", src)
			}
			for _, m := range src.Movements {
				if m.Method != tc.method || m.CardBrand != tc.brand {
					t.Fatalf("movement lineage %+v", m)
				}
			}
			p.ProviderAccount = "reassigned"
			if _, err = New(ctx, db, origin, testConnector, []Program{p}); err == nil {
				t.Fatal("processor silently rebound original acquiring account")
			}
		})
	}
}

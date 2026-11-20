package antomtest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
)

const root = "antom-test/v1/"

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
var currency = regexp.MustCompile(`^[A-Z]{3}$`)
var ledgerID = regexp.MustCompile(`^[a-f0-9]{64}$`)

type enrollment struct {
	Ledger, Host, Client, Account string
	Scope                         scope
}
type record struct {
	Command         railcontract.Command
	RequestID       string
	ParentPaymentID string
	RemoteID        string
	Checkout        string
	LastState       string
	LastObserved    time.Time
}

func (c *Client) valid(cmd railcontract.Command) bool {
	return (scope{cmd.Merchant, cmd.Account, cmd.ConnectorID, cmd.RouteID, cmd.Currency}) == c.scope && cmd.PaymentMethod == "alipay" && cmd.Channel == "hosted" && cmd.Terminal == "" && cmd.ProviderAccount == c.account &&
		idPattern.MatchString(cmd.RouteID) && idPattern.MatchString(cmd.ConnectorID) && idPattern.MatchString(cmd.Account) &&
		idPattern.MatchString(cmd.Merchant) && idPattern.MatchString(cmd.Order) && idPattern.MatchString(cmd.Outlet) &&
		idPattern.MatchString(cmd.ID) && currency.MatchString(cmd.Currency) && cmd.Amount > 0 &&
		((cmd.Kind == "charge" && cmd.Parent == "") || (cmd.Kind == "refund" && idPattern.MatchString(cmd.Parent) && cmd.Parent != cmd.ID))
}
func (c *Client) Identity(ctx context.Context, cmd railcontract.Command) (string, error) {
	if !c.valid(cmd) {
		return "", railcontract.ErrEvidenceConflict
	}
	expected := railcontract.ExpectedProcessorIdentity(ctx)
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	newID := hex.EncodeToString(buf[:])
	var id string
	err := c.db.Update(ctx, func(tx gwstore.Tx) error {
		var e enrollment
		found, err := gwstore.GetJSON(tx, root+"enrollment", &e)
		if err != nil {
			return err
		}
		if !found {
			if expected != "" {
				return railcontract.ErrProcessorIdentityMismatch
			}
			e = enrollment{newID, c.host, c.clientID, c.account, c.scope}
			if err := gwstore.PutJSON(tx, root+"enrollment", e); err != nil {
				return err
			}
		}
		if !ledgerID.MatchString(e.Ledger) || e.Host != c.host || e.Client != c.clientID || e.Account != c.account || e.Scope != c.scope || (expected != "" && expected != e.Ledger) {
			return railcontract.ErrProcessorIdentityMismatch
		}
		id = e.Ledger
		return nil
	})
	return id, err
}

// Every data read and write checks the expected source in its own transaction.
// Financial requests require an explicit ledger identity supplied by the gateway.
func (c *Client) check(tx gwstore.Tx, expected string) error {
	var e enrollment
	ok, err := gwstore.GetJSON(tx, root+"enrollment", &e)
	if err != nil {
		return err
	}
	if !ok || !ledgerID.MatchString(expected) || e.Ledger != expected || e.Host != c.host || e.Client != c.clientID || e.Account != c.account || e.Scope != c.scope {
		return railcontract.ErrProcessorIdentityMismatch
	}
	return nil
}
func recordKey(cmd railcontract.Command, id string) string {
	return root + "ops/" + cmd.Merchant + "/" + cmd.Account + "/" + cmd.RouteID + "/" + id
}
func requestID(ledger string, cmd railcontract.Command) string {
	data, _ := json.Marshal(cmd)
	h := sha256.New()
	h.Write([]byte("dakim-antom-v1/" + ledger + "/"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
func amount(cmd railcontract.Command) money {
	return money{cmd.Currency, strconv.FormatInt(cmd.Amount, 10)}
}

// Antom returns client metadata from inquiryRefund. Bind the original
// provider payment even when the inquiry itself omits paymentId.
func refundBinding(row record) string {
	sum := sha256.Sum256([]byte("dakim-antom-refund/v1/" + row.RequestID + "/" + row.ParentPaymentID))
	return hex.EncodeToString(sum[:])
}

func (c *Client) Submit(ctx context.Context, cmd railcontract.Command) error {
	if !c.valid(cmd) {
		return railcontract.ErrEvidenceConflict
	}
	expected := railcontract.ExpectedProcessorIdentity(ctx)
	row := record{Command: cmd, RequestID: requestID(expected, cmd)}
	inserted := false
	err := c.db.Update(ctx, func(tx gwstore.Tx) error {
		inserted = false
		if err := c.check(tx, expected); err != nil {
			return err
		}
		key := recordKey(cmd, cmd.ID)
		var prior record
		ok, err := gwstore.GetJSON(tx, key, &prior)
		if err != nil {
			return err
		}
		if ok {
			if prior.Command != cmd || prior.RequestID != row.RequestID {
				return railcontract.ErrEvidenceConflict
			}
			return nil
		}
		if cmd.Kind == "refund" {
			var parent record
			ok, err := gwstore.GetJSON(tx, recordKey(cmd, cmd.Parent), &parent)
			if err != nil {
				return err
			}
			base := cmd
			base.Kind = "charge"
			base.ID = cmd.Parent
			base.Parent = ""
			base.Amount = parent.Command.Amount
			if !ok || parent.Command != base || parent.LastState != "captured" || !remoteID.MatchString(parent.RemoteID) || cmd.Amount > base.Amount {
				return railcontract.ErrEvidenceConflict
			}
			row.ParentPaymentID = parent.RemoteID
		}
		inserted = true
		return gwstore.PutJSON(tx, key, row)
	})
	if err != nil || !inserted {
		return err
	}
	var reply response
	if cmd.Kind == "refund" {
		reply, _, err = c.post(ctx, "refund", map[string]any{"metadata": refundBinding(row), "refundRequestId": row.RequestID,
			"paymentId": row.ParentPaymentID, "refundAmount": amount(cmd)})
		if err == nil && (reply.RefundRequestID != "" && reply.RefundRequestID != row.RequestID || reply.PaymentID != "" && reply.PaymentID != row.ParentPaymentID ||
			reply.RefundAmount != (money{}) && reply.RefundAmount != amount(cmd)) {
			return railcontract.ErrEvidenceConflict
		}
		// Standalone refund inquiry supplies the final outcome. An unbound
		// synchronous receipt is never sufficient to credit the merchant.
		return err
	}
	reply, _, err = c.post(ctx, "pay", map[string]any{
		"merchantAccountId": c.account, "productCode": "CASHIER_PAYMENT", "env": map[string]string{"terminalType": "WEB"},
		"paymentRequestId": row.RequestID, "paymentMethod": map[string]string{"paymentMethodType": "ALIPAY_CN"},
		"order":         map[string]any{"referenceOrderId": row.RequestID, "orderDescription": "Merchant checkout", "orderAmount": amount(cmd)},
		"paymentAmount": amount(cmd), "paymentRedirectUrl": c.redirect, "settlementStrategy": map[string]string{"settlementCurrency": cmd.Currency},
	})
	if err != nil {
		return err
	}
	if reply.NormalURL == "" {
		return nil
	} // inquiry still establishes success, failure or unresolved state
	if reply.Result.Status != "U" || reply.Result.Code != "PAYMENT_IN_PROCESS" || reply.PaymentRequestID != row.RequestID ||
		reply.PaymentAmount != amount(cmd) || !remoteID.MatchString(reply.PaymentID) || !CheckoutURL(reply.NormalURL) {
		return railcontract.ErrEvidenceConflict
	}
	return c.db.Update(ctx, func(tx gwstore.Tx) error {
		if err := c.check(tx, expected); err != nil {
			return err
		}
		var prior record
		key := recordKey(cmd, cmd.ID)
		ok, err := gwstore.GetJSON(tx, key, &prior)
		if err != nil {
			return err
		}
		if !ok || prior.Command != cmd || prior.RequestID != row.RequestID || (prior.RemoteID != "" && prior.RemoteID != reply.PaymentID) ||
			(prior.Checkout != "" && prior.Checkout != reply.NormalURL) {
			return railcontract.ErrEvidenceConflict
		}
		prior.RemoteID = reply.PaymentID
		prior.Checkout = reply.NormalURL
		return gwstore.PutJSON(tx, key, prior)
	})
}

func (c *Client) Lookup(ctx context.Context, cmd railcontract.Command) (railcontract.Fact, bool, error) {
	var f railcontract.Fact
	if !c.valid(cmd) {
		return f, false, railcontract.ErrEvidenceConflict
	}
	expected := railcontract.ExpectedProcessorIdentity(ctx)
	key := recordKey(cmd, cmd.ID)
	var row record
	var exists bool
	err := c.db.View(ctx, func(tx gwstore.Tx) error {
		if err := c.check(tx, expected); err != nil {
			return err
		}
		var err error
		exists, err = gwstore.GetJSON(tx, key, &row)
		return err
	})
	if err != nil || !exists {
		return f, false, err
	}
	if row.Command != cmd || row.RequestID != requestID(expected, cmd) {
		return f, false, railcontract.ErrEvidenceConflict
	}
	// Only this ledger originates IDs in its random namespace; once a permit
	// exists, any remote "not found" is inconclusive and cannot permit replay.
	var resp response
	var stamp time.Time
	if cmd.Kind == "charge" {
		resp, stamp, err = c.post(ctx, "inquiryPayment", map[string]string{"merchantAccountId": c.account, "paymentRequestId": row.RequestID})
	} else {
		resp, stamp, err = c.post(ctx, "inquiryRefund", map[string]string{"merchantAccountId": c.account, "refundRequestId": row.RequestID})
	}
	if err != nil {
		return f, false, err
	}
	if resp.Result.Status != "S" || resp.Result.Code != "SUCCESS" {
		return f, false, errors.New("Antom inquiry has no definitive original outcome")
	}
	status, ref := resp.PaymentStatus, resp.PaymentID
	if cmd.Kind == "charge" {
		if resp.PaymentRequestID != row.RequestID || resp.PaymentAmount != amount(cmd) {
			return f, false, railcontract.ErrEvidenceConflict
		}
	} else {
		status, ref = resp.RefundStatus, resp.RefundID
		if !remoteID.MatchString(row.ParentPaymentID) || resp.Metadata != refundBinding(row) || resp.RefundRequestID != row.RequestID || resp.RefundAmount != amount(cmd) ||
			(resp.PaymentID != "" && resp.PaymentID != row.ParentPaymentID) {
			return f, false, railcontract.ErrEvidenceConflict
		}
	}
	if !remoteID.MatchString(ref) || (row.RemoteID != "" && row.RemoteID != ref) {
		return f, false, railcontract.ErrEvidenceConflict
	}
	f.Command = cmd
	switch status {
	case "SUCCESS":
		f.State = "captured"
		if cmd.Kind == "refund" {
			f.State = "refunded"
		}
		f.Reference = ref
	case "FAIL":
		f.State = "declined"
	case "CANCELLED":
		if cmd.Kind != "charge" {
			return railcontract.Fact{}, false, errors.New("unknown refund outcome")
		}
		f.State = "declined"
	case "PENDING", "PROCESSING":
		f.State = "pending"
		if cmd.Kind == "charge" {
			if !CheckoutURL(row.Checkout) {
				return railcontract.Fact{}, false, errors.New("original Antom checkout URL unavailable")
			}
			f.CheckoutURL = row.Checkout
		}
	default:
		return railcontract.Fact{}, false, errors.New("unknown Antom payment state")
	}
	err = c.db.Update(ctx, func(tx gwstore.Tx) error {
		if err := c.check(tx, expected); err != nil {
			return err
		}
		var current record
		ok, err := gwstore.GetJSON(tx, key, &current)
		if err != nil {
			return err
		}
		if !ok || current.Command != cmd || current.RequestID != row.RequestID || (current.RemoteID != "" && current.RemoteID != ref) ||
			stamp.Before(current.LastObserved) || (current.LastState != "" && current.LastState != "pending" && current.LastState != f.State) {
			return railcontract.ErrEvidenceConflict
		}
		current.RemoteID = ref
		current.LastState = f.State
		current.LastObserved = stamp
		return gwstore.PutJSON(tx, key, current)
	})
	if err != nil {
		return railcontract.Fact{}, false, err
	}
	return f, true, nil
}
func (c *Client) InspectRelease(ctx context.Context, cmd railcontract.Command) (railcontract.ReleaseInventory, error) {
	var inv railcontract.ReleaseInventory
	if cmd.Kind != "charge" {
		return inv, railcontract.ErrEvidenceConflict
	}
	f, found, err := c.Lookup(ctx, cmd)
	if err != nil {
		return inv, err
	}
	if !found {
		return inv, errors.New("original Antom payment missing")
	}
	// The online status API supplies no complete independent dispute/refund
	// inventory. A successful wallet payment alone cannot release merchandise.
	return railcontract.ReleaseInventory{Fact: f, Complete: false, ObservedAt: c.now().UTC(), DisputeStatus: "unknown"}, nil
}

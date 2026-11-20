package virtualrail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"dakimpay/internal/railcontract"
)

type Client struct {
	origin, token string
	http          *http.Client
}

func NewClient(origin, token string) (*Client, error) {
	if _, err := checkOrigin(origin); err != nil {
		return nil, err
	}
	if len(token) < 16 {
		return nil, errors.New("processor credential is missing or too short")
	}
	return &Client{origin, token, &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) call(ctx context.Context, method, path string, payload []byte) (int, []byte, error) {
	expected := railcontract.ExpectedProcessorIdentity(ctx)
	if !ledgerPattern.MatchString(expected) {
		return 0, nil, railcontract.ErrProcessorIdentityMismatch
	}
	status, body, _, err := c.request(ctx, method, path, payload, expected)
	return status, body, err
}

func (c *Client) request(ctx context.Context, method, path string, payload []byte, expected string) (int, []byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.origin+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if expected != "" {
		req.Header.Set(ledgerHeader, expected)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 256*1024+1))
	if err != nil {
		return res.StatusCode, nil, "", err
	}
	ids := res.Header.Values(ledgerHeader)
	if len(ids) != 1 || !ledgerPattern.MatchString(ids[0]) {
		return res.StatusCode, nil, "", errors.New("processor response missing an unambiguous ledger identity")
	}
	if (expected != "" && ids[0] != expected) || res.StatusCode == http.StatusPreconditionFailed {
		return res.StatusCode, nil, "", railcontract.ErrProcessorIdentityMismatch
	}
	if len(body) > 256*1024 {
		if res.StatusCode == http.StatusOK {
			return res.StatusCode, nil, ids[0], railcontract.ErrEvidenceConflict
		}
		return res.StatusCode, nil, ids[0], errors.New("oversized processor response")
	}
	return res.StatusCode, body, ids[0], nil
}

func (c *Client) Submit(ctx context.Context, command railcontract.Command) error {
	p, err := json.Marshal(command)
	if err != nil {
		return err
	}
	code, _, err := c.call(ctx, http.MethodPost, "/rail/v1/operations", p)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("processor submission returned HTTP %d; use original operation inquiry", code)
	}
	// An accepted submission is not authority to post. Always query the original.
	return nil
}

// Reject duplicate fields, case aliases, uncontrolled nesting and trailing JSON
// before typed decoding. Both single-operation and release-inventory evidence
// use this parser so a partial or ambiguous reply cannot authorize a receipt.
func checkJSON(d *json.Decoder, depth int) error {
	if depth > 4 {
		return errors.New("processor evidence too deeply nested")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	switch t {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			tok, err := d.Token()
			if err != nil {
				return err
			}
			k, ok := tok.(string)
			if !ok || seen[k] || len(seen) >= 40 || len(k) == 0 {
				return errors.New("duplicate or invalid processor field")
			}
			for _, r := range k {
				if (r < 'a' || r > 'z') && r != '_' {
					return errors.New("invalid processor field name")
				}
			}
			seen[k] = true
			if err := checkJSON(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	case json.Delim('['):
		for n := 0; d.More(); n++ {
			if n >= 250 {
				return errors.New("processor inventory too large")
			}
			if err := checkJSON(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	case nil:
		return errors.New("null processor evidence")
	default:
		return nil
	}
}
func decodeEvidence(raw []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := checkJSON(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("invalid trailing processor evidence")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}
func (c *Client) Lookup(ctx context.Context, cmd railcontract.Command) (railcontract.Fact, bool, error) {
	var f railcontract.Fact
	path := "/rail/v1/accounts/" + url.PathEscape(cmd.Account) + "/merchants/" + url.PathEscape(cmd.Merchant) + "/operations/" + url.PathEscape(cmd.ID)
	code, body, err := c.call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return f, false, err
	}
	if code == 404 {
		var miss absence
		if decodeEvidence(body, &miss) == nil && miss == (absence{cmdRoute(cmd), "operation_absent", cmd.Merchant, cmd.Account, cmd.ID}) {
			return f, false, nil
		}
	}
	if code != 200 {
		return f, false, fmt.Errorf("inconclusive original operation lookup: HTTP %d", code)
	}
	if err = decodeEvidence(body, &f); err != nil {
		return railcontract.Fact{}, false, railcontract.ErrEvidenceConflict
	}
	if err = c.validateFact(f, cmd); err != nil {
		return f, false, railcontract.ErrEvidenceConflict
	}
	return f, true, nil
}
func (c *Client) validateFact(f railcontract.Fact, cmd railcontract.Command) error {
	if f.Command != cmd {
		return railcontract.ErrEvidenceConflict
	}
	if f.CardBrand != "" && (!cardBrand(f.CardBrand) || (cmd.PaymentMethod != "card" && cmd.PaymentMethod != f.CardBrand) || (f.State != "captured" && f.State != "refunded")) {
		return errors.New("invalid processor card brand")
	}
	if (cmd.PaymentMethod == "card" || cardBrand(cmd.PaymentMethod)) && (f.State == "captured" || f.State == "refunded") && f.CardBrand == "" {
		return errors.New("missing processor card-brand observation")
	}
	switch f.State {
	case "captured", "refunded":
		if (f.State == "refunded") != (cmd.Kind == "refund") || !validID.MatchString(f.Reference) || f.CheckoutURL != "" {
			return errors.New("invalid processor movement identity")
		}
	case "declined":
		if f.Reference != "" || f.CheckoutURL != "" {
			return errors.New("invalid declined operation")
		}
	case "pending":
		if cmd.Kind != "charge" || f.Reference != "" {
			return errors.New("invalid pending operation")
		}
		if cmd.Channel == "hosted" {
			u, err := url.Parse(f.CheckoutURL)
			if err != nil || !validCapability.MatchString(lastPathSegment(u.Path)) || f.CheckoutURL != c.origin+"/checkout/"+lastPathSegment(u.Path) {
				return errors.New("invalid checkout destination")
			}
		} else if f.CheckoutURL != "" {
			return errors.New("invalid terminal redirect")
		}
	default:
		return errors.New("unknown processor outcome")
	}
	return nil
}
func lastPathSegment(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

var _ railcontract.Processor = (*Client)(nil)

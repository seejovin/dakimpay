package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"

	"dakimpay/internal/gwstore"
)

type financeAction func(http.ResponseWriter, *http.Request, Principal) error

func financeRoute(fn financeAction, roles ...Role) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		p, ok := PrincipalFrom(r.Context())
		var err error
		switch {
		case !ok || p.Subject == "" || p.Merchant == "" || p.Account == "" || p.Currency == "" || p.Beneficiary == "":
			err = financialError{401, "program credential required"}
		default:
			allowed := false
			for _, role := range roles {
				if p.Role == role {
					allowed = true
					break
				}
			}
			if allowed {
				err = fn(w, r, p)
			} else {
				err = financialError{403, "credential lacks this authority"}
			}
		}
		if err == nil {
			return
		}
		code, message := http.StatusInternalServerError, "financial operation failed"
		var e financialError
		var enrollment engineError
		if errors.As(err, &e) {
			code, message = e.status, e.msg
		} else if errors.As(err, &enrollment) {
			code, message = enrollment.status, enrollment.code
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
	}
}

func (s *Service) registerFinance(m *http.ServeMux) {
	m.HandleFunc("POST /v2/settlement-fee-corrections", financeRoute(s.financeFeeCorrection, StatementRole))
	m.HandleFunc("GET /v2/routes/{route}/accounts/{providerAccount}/payouts/{id}/confirmed-fee-correction", financeRoute(s.financeConfirmedFeeCorrection, MerchantRole, FinanceRole, ReviewerRole))
	m.HandleFunc("POST /v1/settlement-statements", financeRoute(s.financeStatement, StatementRole))
	m.HandleFunc("POST /v1/bank-entries", financeRoute(s.financeBank, BankRole))
	for _, path := range []string{"/v1/payouts/{id}", "/v1/routes/{route}/accounts/{providerAccount}/payouts/{id}"} {
		m.HandleFunc("POST "+path+"/prepare-close", financeRoute(s.financePrepare, FinanceRole))
		m.HandleFunc("POST "+path+"/approve-close", financeRoute(s.financeApprove, ReviewerRole))
		m.HandleFunc("GET "+path, financeRoute(s.financePayout, MerchantRole, FinanceRole, ReviewerRole))
	}
	m.HandleFunc("GET /v1/journal", financeRoute(s.financeJournal, MerchantRole, FinanceRole, ReviewerRole))
}

var financeField = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// A manifest may not acquire different terms through duplicate fields or
// case-insensitive JSON aliases. Read at most one bounded object.
func financeJSON(r *http.Request, target any, required ...string) error {
	const max = 256 << 10
	raw, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return financialError{400, "could not read financial input"}
	}
	if len(raw) > max {
		return financialError{413, "financial input too large"}
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' || !json.Valid(raw) {
		return financialError{400, "expected one JSON object"}
	}
	wire := json.NewDecoder(bytes.NewReader(raw))
	wire.UseNumber()
	fields := map[string]bool{}
	if !financeUnique(wire, fields, 0) {
		return financialError{400, "invalid, null, aliased or repeated field"}
	}
	for _, k := range required {
		if !fields[k] {
			return financialError{400, "missing field " + k}
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(target); err != nil {
		return financialError{400, "unrecognized financial input"}
	}
	return nil
}

func financeUnique(d *json.Decoder, top map[string]bool, depth int) bool {
	if depth > 4 {
		return false
	}
	t, err := d.Token()
	if err != nil || t == nil {
		return false
	}
	switch t {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			s, ok := k.(string)
			if e != nil || !ok || !financeField.MatchString(s) || seen[s] {
				return false
			}
			seen[s] = true
			if depth == 0 {
				top[s] = true
			}
			if !financeUnique(d, top, depth+1) {
				return false
			}
		}
	case json.Delim('['):
		for d.More() {
			if !financeUnique(d, top, depth+1) {
				return false
			}
		}
	default:
		return true
	}
	_, err = d.Token()
	return err == nil
}

func financeResponse(w http.ResponseWriter, status int, result any) error {
	w.WriteHeader(status)
	// The financial transaction has already committed. A client losing this
	// response can replay the immutable source identity.
	_ = json.NewEncoder(w).Encode(result)
	return nil
}

func (s *Service) financeStatement(w http.ResponseWriter, r *http.Request, p Principal) error {
	var in SettlementStatement
	if err := financeJSON(r, &in, "net_minor"); err != nil {
		return err
	}
	if err := in.validate(p); err != nil {
		return err
	}
	var out payoutRecord
	var created bool
	now := s.Now()
	err := s.Store.Update(r.Context(), func(tx gwstore.Tx) error {
		var err error
		out, created, err = importStatement(tx, p, in, now)
		return err
	})
	if err != nil {
		return err
	}
	if created {
		return financeResponse(w, 201, out)
	}
	return financeResponse(w, 200, out)
}
func (s *Service) financeBank(w http.ResponseWriter, r *http.Request, p Principal) error {
	var in BankEntry
	if err := financeJSON(r, &in); err != nil {
		return err
	}
	if err := in.validate(p); err != nil {
		return err
	}
	var out bankRecord
	var created bool
	now := s.Now()
	err := s.Store.Update(r.Context(), func(tx gwstore.Tx) error {
		var err error
		out, created, err = importBank(tx, p, in, now)
		return err
	})
	if err != nil {
		return err
	}
	if created {
		return financeResponse(w, 201, out)
	}
	return financeResponse(w, 200, out)
}
func payoutRoute(r *http.Request) FinancialRoute {
	return FinancialRoute{RouteID: r.PathValue("route"), ProviderAccount: r.PathValue("providerAccount")}
}

func (s *Service) financePrepare(w http.ResponseWriter, r *http.Request, p Principal) error {
	var in struct {
		BankEntryID string `json:"bank_entry_id"`
	}
	if err := financeJSON(r, &in, "bank_entry_id"); err != nil {
		return err
	}
	var out payoutRecord
	now := s.Now()
	err := s.Store.Update(r.Context(), func(tx gwstore.Tx) error {
		var err error
		out, err = prepareClose(tx, p, payoutRoute(r), r.PathValue("id"), in.BankEntryID, now)
		return err
	})
	if err != nil {
		return err
	}
	return financeResponse(w, 200, out)
}
func (s *Service) financeApprove(w http.ResponseWriter, r *http.Request, p Principal) error {
	var in struct {
		ProposalID string `json:"proposal_id"`
	}
	if err := financeJSON(r, &in, "proposal_id"); err != nil {
		return err
	}
	var out payoutRecord
	now := s.Now()
	err := s.Store.Update(r.Context(), func(tx gwstore.Tx) error {
		var err error
		out, err = approveClose(tx, p, payoutRoute(r), r.PathValue("id"), in.ProposalID, now)
		return err
	})
	if err != nil {
		return err
	}
	return financeResponse(w, 200, out)
}
func (s *Service) financePayout(w http.ResponseWriter, r *http.Request, p Principal) error {
	var out payoutRecord
	err := s.Store.View(r.Context(), func(tx gwstore.Tx) error {
		var err error
		out, err = payoutByID(tx, p, payoutRoute(r), r.PathValue("id"))
		return err
	})
	if err != nil {
		return err
	}
	return financeResponse(w, 200, out)
}

func (s *Service) financeJournal(w http.ResponseWriter, r *http.Request, p Principal) error {
	query := r.URL.Query()
	get := func(name string, def int64) (int64, error) {
		values, ok := query[name]
		if !ok {
			return def, nil
		}
		if len(values) != 1 {
			return 0, financialInvalid("repeated " + name)
		}
		n, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || n < 0 {
			return 0, financialInvalid("invalid " + name)
		}
		return n, nil
	}
	after, err := get("after", 0)
	if err != nil {
		return err
	}
	limit, err := get("limit", 100)
	if err != nil {
		return err
	}
	if limit < 1 || limit > 250 {
		return financialInvalid("limit must be between 1 and 250")
	}
	through, err := get("through", -1)
	if err != nil {
		return err
	}
	type page struct {
		Entries []journalEntry `json:"entries"`
		Through int64          `json:"through_sequence"`
		Next    int64          `json:"next_after"`
	}
	var out page
	prefix := bookPrefix(p.Merchant, p.Account, p.Currency)
	err = s.Store.View(r.Context(), func(tx gwstore.Tx) error {
		if err := checkFinanceAuthority(tx, p, false); err != nil {
			return err
		}
		out = page{Entries: []journalEntry{}, Next: after}
		var last int64
		if _, err := gwstore.GetJSON(tx, prefix+"sequence", &last); err != nil {
			return err
		}
		out.Through = last
		if through >= 0 {
			if through > last {
				return financialInvalid("unknown journal watermark")
			}
			out.Through = through
		}
		if after > out.Through {
			return financialInvalid("cursor exceeds journal watermark")
		}
		if after == out.Through {
			return nil
		}
		pageSize := min(limit, out.Through-after)
		rows, err := tx.Scan(prefix+"journal/", entryKey(prefix, after), int(pageSize))
		if err != nil {
			return err
		}
		for _, row := range rows {
			var entry journalEntry
			if err := json.Unmarshal(row.Value, &entry); err != nil {
				return err
			}
			if entry.Sequence != out.Next+1 {
				return errors.New("journal sequence gap")
			}
			out.Entries = append(out.Entries, entry)
			out.Next = entry.Sequence
		}
		if int64(len(rows)) != pageSize {
			return errors.New("journal snapshot incomplete")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return financeResponse(w, 200, out)
}

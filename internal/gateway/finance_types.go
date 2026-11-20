package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"sort"
	"time"
)

// All accounts in this book describe the merchant's economic position. The
// gateway's corporate cash and revenue never enter this book.
const (
	processorClearing  = "processor_clearing"
	merchantProceeds   = "merchant_proceeds"
	fundingInTransit   = "funding_in_transit"
	bankObserved       = "observed_merchant_bank"
	bankUnallocated    = "unallocated_bank_movements"
	bankReturnSuspense = "bank_return_under_investigation"
)

type financialError struct {
	status int
	msg    string
}

func (e financialError) Error() string   { return e.msg }
func financialConflict(msg string) error { return financialError{http.StatusConflict, msg} }
func financialInvalid(msg string) error  { return financialError{http.StatusUnprocessableEntity, msg} }

var financialID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_:.\-]{0,127}$`)
var financialCurrency = regexp.MustCompile(`^[A-Z]{3}$`)
var financialDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func financeHash(v any) string {
	// Only closed structs / string lists with JSON-compatible fields are hashed.
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type StatementLine struct {
	LineID    string `json:"line_id"`
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Parent    string `json:"parent_reference,omitempty"`
	Amount    int64  `json:"amount_minor"`
}

// FinancialRoute identifies a processor's native reference namespace within a
// merchant book. Empty components are the historical single-connector namespace.
type FinancialRoute struct {
	RouteID         string `json:"route_id,omitempty"`
	ProviderAccount string `json:"provider_account,omitempty"`
}

func (r FinancialRoute) valid() bool {
	return r == (FinancialRoute{}) || (financialID.MatchString(r.RouteID) && financialID.MatchString(r.ProviderAccount))
}

func (r FinancialRoute) sourcePrefix(book string) string {
	if r == (FinancialRoute{}) {
		return book
	}
	return book + "routes/" + financeHash([]string{r.RouteID, r.ProviderAccount}) + "/"
}

type SettlementStatement struct {
	FinancialRoute
	ID          string          `json:"payout_id"`
	Trace       string          `json:"bank_trace"`
	Beneficiary string          `json:"beneficiary"`
	Currency    string          `json:"currency"`
	Net         int64           `json:"net_minor"`
	Source      string          `json:"source_digest"`
	Lines       []StatementLine `json:"lines"`
}

type BankEntry struct {
	ID          string `json:"entry_id"`
	Trace       string `json:"bank_trace"`
	Beneficiary string `json:"beneficiary"`
	Currency    string `json:"currency"`
	Amount      int64  `json:"amount_minor"`
	Source      string `json:"source_digest"`
	ReturnOf    string `json:"return_of_bank_entry,omitempty"`
}

type payoutRecord struct {
	SettlementStatement
	Correction            *bookedFeeCorrection `json:"fee_correction,omitempty"`
	Merchant              string               `json:"merchant_id"`
	Account               string               `json:"processor_account"`
	ImportedBy            string               `json:"imported_by"`
	ImportedAt            time.Time            `json:"imported_at"`
	Status                string               `json:"status"`
	Exceptions            []string             `json:"exception_ids,omitempty"`
	OpenExceptions        []string             `json:"open_exceptions,omitempty"`
	FundingMatchAmbiguous bool                 `json:"funding_match_ambiguous,omitempty"`
	Returned              int64                `json:"returned_minor,omitempty"`
	ProcessorRecredit     int64                `json:"processor_recredit_minor,omitempty"`
	Proposal              string               `json:"proposal_id,omitempty"`
	BankEntry             string               `json:"bank_entry_id,omitempty"`
	PreparedBy            string               `json:"prepared_by,omitempty"`
	PreparedAt            *time.Time           `json:"prepared_at,omitempty"`
	ApprovedBy            string               `json:"approved_by,omitempty"`
	ApprovedAt            *time.Time           `json:"approved_at,omitempty"`
}

type bankRecord struct {
	BankEntry
	Merchant   string    `json:"merchant_id"`
	Account    string    `json:"processor_account"`
	ImportedBy string    `json:"imported_by"`
	ImportedAt time.Time `json:"imported_at"`
}

type journalLeg struct {
	Account string `json:"account"`
	Debit   int64  `json:"debit_minor"`
	Credit  int64  `json:"credit_minor"`
}
type journalEntry struct {
	FinancialRoute
	PaymentMethod string        `json:"payment_method,omitempty"`
	ConnectorID   string        `json:"connector_id,omitempty"`
	CardBrand     string        `json:"card_brand,omitempty"`
	Sequence      int64         `json:"sequence"`
	Merchant      string        `json:"merchant_id"`
	Account       string        `json:"processor_account"`
	Currency      string        `json:"currency"`
	Kind          string        `json:"kind"`
	Reference     string        `json:"reference"`
	Parent        string        `json:"parent_reference,omitempty"`
	Operation     string        `json:"operation_id,omitempty"`
	Order         string        `json:"order_id,omitempty"`
	Source        string        `json:"source_digest,omitempty"`
	Template      string        `json:"template"`
	BookedAt      time.Time     `json:"booked_at"`
	Legs          [2]journalLeg `json:"legs"`
}

func financialLegs(debit, credit string, signedAmount int64) ([2]journalLeg, error) {
	if signedAmount == 0 || signedAmount == math.MinInt64 || debit == credit {
		return [2]journalLeg{}, financialInvalid("invalid posting")
	}
	if signedAmount < 0 {
		debit, credit = credit, debit
		signedAmount = -signedAmount
	}
	return [2]journalLeg{{Account: debit, Debit: signedAmount}, {Account: credit, Credit: signedAmount}}, nil
}

func addFinancialAmount(total, amount int64) (int64, error) {
	if (amount > 0 && total > math.MaxInt64-amount) || (amount < 0 && total < math.MinInt64-amount) {
		return 0, financialInvalid("statement total exceeds supported precision")
	}
	return total + amount, nil
}

func (x *SettlementStatement) validate(p Principal) error {
	if !x.FinancialRoute.valid() || !financialID.MatchString(x.ID) || !financialID.MatchString(x.Trace) ||
		!financialDigest.MatchString(x.Source) || x.Beneficiary != p.Beneficiary || x.Beneficiary == "" ||
		x.Currency != p.Currency || !financialCurrency.MatchString(x.Currency) ||
		len(x.Lines) == 0 || len(x.Lines) > 250 || x.Net == math.MinInt64 {
		return financialInvalid("invalid statement identity, program, digest, or line count")
	}
	// Compare an immutable manifest independent of JSON formatting and line ordering.
	sort.Slice(x.Lines, func(i, j int) bool { return x.Lines[i].LineID < x.Lines[j].LineID })
	var total int64
	seen := make(map[[2]string]bool)
	for i, l := range x.Lines {
		movementKind := l.Kind
		if movementKind == "additional_capture" {
			movementKind = "capture"
		} else if movementKind == "refund_excess_capture" || movementKind == "source_refund" {
			movementKind = "refund"
		}
		key := [2]string{movementKind, l.Reference}
		if !financialID.MatchString(l.LineID) || !financialID.MatchString(l.Reference) ||
			l.Amount == 0 || l.Amount == math.MinInt64 || (i > 0 && x.Lines[i-1].LineID == l.LineID) || seen[key] {
			return financialInvalid("invalid or duplicate statement line")
		}
		seen[key] = true
		switch l.Kind {
		case "capture", "refund":
			if l.Parent != "" || (l.Kind == "capture") != (l.Amount > 0) {
				return financialInvalid("invalid payment line direction or parent")
			}
		case "return_recredit":
			if !financialID.MatchString(l.Parent) {
				return financialInvalid("bank return reference required")
			}
		case "additional_capture", "refund_excess_capture", "source_refund":
			if !financialID.MatchString(l.Parent) || l.Parent == l.Reference || (l.Kind == "additional_capture") != (l.Amount > 0) {
				return financialInvalid("an excess movement requires a distinct reference, parent and correct direction")
			}
		case "fee", "fee_rebate", "chargeback", "chargeback_reversal":
			positive := l.Kind == "fee_rebate" || l.Kind == "chargeback_reversal"
			if !financialID.MatchString(l.Parent) || positive != (l.Amount > 0) {
				return financialInvalid("invalid adjustment direction or parent")
			}
		default:
			return financialInvalid("unsupported statement line kind")
		}
		var err error
		total, err = addFinancialAmount(total, l.Amount)
		if err != nil {
			return err
		}
	}
	if total != x.Net {
		return financialInvalid("statement lines do not explain the stated net")
	}
	return nil
}

func (b BankEntry) validate(p Principal) error {
	if !financialID.MatchString(b.ID) || !financialID.MatchString(b.Trace) ||
		b.Beneficiary == "" || b.Beneficiary != p.Beneficiary || b.Currency != p.Currency ||
		!financialCurrency.MatchString(b.Currency) || !financialDigest.MatchString(b.Source) ||
		b.Amount == 0 || b.Amount == math.MinInt64 || b.ReturnOf != "" && (!financialID.MatchString(b.ReturnOf) || b.ReturnOf == b.ID) {
		return financialInvalid("invalid independently posted bank entry")
	}
	return nil
}

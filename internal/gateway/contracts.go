package gateway

import (
	"context"
	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
	"net/http"
	"time"
)

type Role string

const (
	MerchantRole  Role = "merchant"
	StatementRole Role = "statement"
	BankRole      Role = "bank"
	FinanceRole   Role = "finance"
	ReviewerRole  Role = "reviewer"
)

type Principal struct {
	Merchant string
	Role     Role
	Subject  string
	// Immutable configured authority of this merchant program, derived from the credential.
	Account                  string
	Beneficiary              string
	Currency                 string
	Outlets                  map[string]bool
	Terminals                map[string]string // terminal -> outlet
	MaxAmount                int64
	StatementRoute           string `json:",omitempty"`
	StatementProviderAccount string `json:",omitempty"`
	// Verification keys are public and belong to this named statement-source grant.
	FeeCorrectionKeys map[string]string `json:",omitempty"`
}
type principalKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

type Service struct {
	Store           gwstore.Store
	SelectRoute     func(railcontract.Command) (railcontract.Command, error)
	RouteCandidates func(railcontract.Command) []railcontract.Command
	RouteExists     func(merchant, account, routeID string) bool
	CanDispatch     func(railcontract.Command) bool
	CurrentProgram  func(merchant string) (Principal, bool)
	Rail            railcontract.Processor
	Now             func() time.Time // always wired
}

func New(store gwstore.Store, rail railcontract.Processor) *Service {
	return &Service{Store: store, Rail: rail, Now: time.Now}
}
func (s *Service) Handler() http.Handler {
	m := http.NewServeMux()
	s.registerPayments(m)
	s.registerFinance(m)
	s.registerControl(m)
	s.RegisterDelivery(m)
	return m
}

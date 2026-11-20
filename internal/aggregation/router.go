// Package aggregation binds merchant payments to an enrolled processor route.
// Selection is a pure admission decision. Every subsequent financial operation
// addresses the route recorded on the original command.
package aggregation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"

	"dakimpay/internal/railcontract"
)

// Route identifies one processor enrollment in one gateway merchant program.
// Lower positive priorities win; overlapping active rules must have distinct
// priorities. Amount limits are inclusive integer minor units.
type Route struct {
	ID              string   `json:"id"`
	Merchant        string   `json:"merchant"`
	Account         string   `json:"account"`
	Connector       string   `json:"connector"`
	ProviderAccount string   `json:"provider_account"`
	Currency        string   `json:"currency"`
	Methods         []string `json:"methods"`
	Channels        []string `json:"channels"`
	MinAmount       int64    `json:"min_amount_minor"`
	MaxAmount       int64    `json:"max_amount_minor"`
	Priority        int      `json:"priority"`
	Enabled         bool     `json:"enabled"`
}

type program struct{ merchant, account string }

type processor interface {
	railcontract.Processor
	railcontract.ProcessorIdentity
	railcontract.ReleaseInspector
}

// Router is a read-only snapshot. Rebuild it when configuration is reloaded.
// The caller cannot mutate a route after New returns.
type Router struct {
	programs map[program][]Route
	byID     map[program]map[string]Route
	clients  map[string]processor
	legacy   processor
}

var (
	ErrNoEligibleRoute = errors.New("no eligible payment route")
	ErrPinnedRoute     = errors.New("original payment route unavailable or inconsistent")
	idPattern          = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,95}$`)
	currencyPattern    = regexp.MustCompile(`^[A-Z]{3}$`)
	knownMethods       = []string{"card", "visa", "mastercard", "amex", "wechat_pay", "alipay"}
	knownChannels      = []string{"hosted", "terminal"}
)

// New rejects ambiguous selections and connectors missing inquiry, original
// financial history identity, or release evidence. legacy, if supplied, is used
// only for historical records created before the route fields existed.
func New(routes []Route, processors map[string]railcontract.Processor, legacy railcontract.Processor) (*Router, error) {
	if len(routes) == 0 || len(routes) > 4096 {
		return nil, errors.New("route catalog requires 1 through 4096 enrollments")
	}
	router := &Router{programs: make(map[program][]Route), byID: make(map[program]map[string]Route), clients: make(map[string]processor)}
	for name, p := range processors {
		client, ok := qualified(p)
		if !idPattern.MatchString(name) || !ok {
			return nil, fmt.Errorf("invalid or incomplete connector %q", name)
		}
		router.clients[name] = client
	}
	if legacy != nil {
		var ok bool
		if router.legacy, ok = qualified(legacy); !ok {
			return nil, errors.New("incomplete historical connector")
		}
	}
	// The direct-settlement model has one independent source account per
	// merchant program on a connector. Shared-MID PayFac flows need an
	// independently evidenced submerchant identity.
	type downstream struct{ connector, account string }
	sources := make(map[downstream]bool)
	for _, route := range routes {
		if !idPattern.MatchString(route.ID) || !idPattern.MatchString(route.Merchant) || !idPattern.MatchString(route.Account) ||
			!idPattern.MatchString(route.ProviderAccount) || router.clients[route.Connector] == nil || !currencyPattern.MatchString(route.Currency) ||
			route.MinAmount < 1 || route.MaxAmount < route.MinAmount || route.Priority < 1 ||
			!validSet(route.Methods, knownMethods) || !validSet(route.Channels, knownChannels) {
			return nil, fmt.Errorf("invalid enrollment for route %q", route.ID)
		}
		if slices.Contains(route.Channels, "terminal") && (slices.Contains(route.Methods, "alipay") || slices.Contains(route.Methods, "wechat_pay")) {
			return nil, fmt.Errorf("wallet presentation requires a hosted route: %q", route.ID)
		}
		source := downstream{route.Connector, route.ProviderAccount}
		if sources[source] {
			return nil, fmt.Errorf("downstream account is bound to multiple routes: %q", route.ID)
		}
		sources[source] = true
		key := program{route.Merchant, route.Account}
		if router.byID[key] == nil {
			router.byID[key] = make(map[string]Route)
		}
		if _, exists := router.byID[key][route.ID]; exists {
			return nil, fmt.Errorf("duplicate merchant route %q", route.ID)
		}
		for _, prior := range router.programs[key] {
			if prior.Enabled && route.Enabled && prior.Priority == route.Priority && prior.Currency == route.Currency &&
				prior.MinAmount <= route.MaxAmount && route.MinAmount <= prior.MaxAmount &&
				intersects(prior.Methods, route.Methods) && intersects(prior.Channels, route.Channels) {
				return nil, fmt.Errorf("equal-priority routes %q and %q overlap", prior.ID, route.ID)
			}
		}
		route.Methods = slices.Clone(route.Methods)
		route.Channels = slices.Clone(route.Channels)
		router.byID[key][route.ID] = route
		router.programs[key] = append(router.programs[key], route)
	}
	for _, rows := range router.programs {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Priority == rows[j].Priority {
				return rows[i].ID < rows[j].ID
			}
			return rows[i].Priority < rows[j].Priority
		})
	}
	return router, nil
}

func qualified(p railcontract.Processor) (processor, bool) {
	impl, ok := p.(processor)
	if !ok {
		return nil, false
	}
	v := reflect.ValueOf(impl)
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Func, reflect.Slice, reflect.Chan:
		if v.IsNil() {
			return nil, false
		}
	}
	return impl, true
}

func validSet(values, allowed []string) bool {
	if len(values) == 0 || len(values) > len(allowed) {
		return false
	}
	for i, value := range values {
		if !slices.Contains(allowed, value) || slices.Contains(values[:i], value) {
			return false
		}
	}
	return true
}

func intersects(a, b []string) bool {
	for _, s := range a {
		if slices.Contains(b, s) {
			return true
		}
	}
	return false
}

func (r Route) offers(currency, channel, method string, amount int64) bool {
	return r.Enabled && r.Currency == currency && slices.Contains(r.Channels, channel) && slices.Contains(r.Methods, method) &&
		(amount == 0 || (r.MinAmount <= amount && amount <= r.MaxAmount))
}

// Methods returns only customer-facing methods eligible for the given program
// and channel. An amount of zero asks which methods are offered for some valid
// amount; admission always requires a concrete positive amount.
func (r *Router) Methods(merchant, account, currency, channel string, amount int64) []string {
	available := make([]string, 0, len(knownMethods))
	for _, method := range knownMethods {
		for _, route := range r.programs[program{merchant, account}] {
			if route.offers(currency, channel, method, amount) {
				available = append(available, method)
				break
			}
		}
	}
	return available
}

// Select is called only for a new charge. It never calls a processor and never
// accepts a merchant-supplied route. Omitting the payment method requests card.
func (r *Router) Select(c railcontract.Command) (railcontract.Command, error) {
	rows := r.RouteCandidates(c)
	if len(rows) == 0 {
		return railcontract.Command{}, ErrNoEligibleRoute
	}
	return rows[0], nil
}

func unbound(c railcontract.Command) bool {
	return c.RouteID == "" && c.ConnectorID == "" && c.ProviderAccount == "" && c.PaymentMethod == ""
}

func (r *Router) original(c railcontract.Command) (processor, Route, error) {
	if unbound(c) && r.legacy != nil {
		return r.legacy, Route{}, nil
	}
	route, ok := r.byID[program{c.Merchant, c.Account}][c.RouteID]
	if !ok || route.Connector != c.ConnectorID || route.ProviderAccount != c.ProviderAccount ||
		route.Currency != c.Currency || !slices.Contains(knownMethods, c.PaymentMethod) {
		return nil, Route{}, ErrPinnedRoute
	}
	return r.clients[route.Connector], route, nil
}

// CanDispatch applies the current sales policy before issuing a new physical
// submission. Requiring a refund's original parent and connector preserves
// return access when sales were suspended or acceptance limits changed.
func (r *Router) CanDispatch(c railcontract.Command) bool {
	_, route, err := r.original(c)
	if err != nil || c.Amount < 1 || !slices.Contains(knownChannels, c.Channel) {
		return false
	}
	if c.Kind == "refund" {
		return c.Parent != ""
	}
	return c.Kind == "charge" && c.Parent == "" && !unbound(c) && route.offers(c.Currency, c.Channel, c.PaymentMethod, c.Amount)
}

func (r *Router) Identity(ctx context.Context, c railcontract.Command) (string, error) {
	client, _, err := r.original(c)
	if err != nil {
		return "", err
	}
	return client.Identity(ctx, c)
}

func (r *Router) Submit(ctx context.Context, c railcontract.Command) error {
	client, _, err := r.original(c)
	if err != nil {
		return err
	}
	if !r.CanDispatch(c) {
		return ErrPinnedRoute
	}
	return client.Submit(ctx, c)
}

func (r *Router) Lookup(ctx context.Context, c railcontract.Command) (railcontract.Fact, bool, error) {
	client, _, err := r.original(c)
	if err != nil {
		return railcontract.Fact{}, false, err
	}
	fact, found, err := client.Lookup(ctx, c)
	if err == nil && found && fact.Command != c {
		return railcontract.Fact{}, false, railcontract.ErrEvidenceConflict
	}
	return fact, found, err
}

func (r *Router) InspectRelease(ctx context.Context, c railcontract.Command) (railcontract.ReleaseInventory, error) {
	client, _, err := r.original(c)
	if err != nil {
		return railcontract.ReleaseInventory{}, err
	}
	inventory, err := client.InspectRelease(ctx, c)
	if err == nil && inventory.Fact.Command != c {
		return railcontract.ReleaseInventory{}, railcontract.ErrEvidenceConflict
	}
	if err == nil {
		for _, refund := range inventory.Refunds {
			if refund.Merchant != c.Merchant || refund.Account != c.Account || refund.Currency != c.Currency ||
				refund.Kind != "refund" || refund.Parent != c.ID || refund.RouteID != c.RouteID ||
				refund.ConnectorID != c.ConnectorID || refund.ProviderAccount != c.ProviderAccount || refund.PaymentMethod != c.PaymentMethod {
				return railcontract.ReleaseInventory{}, railcontract.ErrEvidenceConflict
			}
		}
	}
	return inventory, err
}

var _ processor = (*Router)(nil)

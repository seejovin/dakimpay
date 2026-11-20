package virtualrail

import (
	"context"
	"errors"
	"net/http"

	"dakimpay/internal/railcontract"
	"github.com/jackc/pgx/v5"
)

const fields = `merchant,account,id,kind,parent,order_id,outlet,terminal,channel,amount,currency,state,movement,coalesce(hosted_capability,''),payment_method,route_id,connector_id,provider_account,card_brand`
const original = `merchant=$1 AND account=$2 AND id=$3`

func (s *service) scan(row pgx.Row) (railcontract.Fact, error) {
	var f railcontract.Fact
	var capability string
	err := row.Scan(&f.Merchant, &f.Account, &f.ID, &f.Kind, &f.Parent, &f.Order, &f.Outlet, &f.Terminal, &f.Channel, &f.Amount, &f.Currency, &f.State, &f.Reference, &capability, &f.PaymentMethod, &f.RouteID, &f.ConnectorID, &f.ProviderAccount, &f.CardBrand)
	if err == nil && f.State == "pending" && capability != "" {
		f.CheckoutURL = s.origin + "/checkout/" + capability
	}
	return f, err
}
func (s *service) validate(c railcontract.Command) error {
	p, ok := s.programs[c.Account]
	if !ok || p.Merchant != c.Merchant || !acceptsRoute(p, c) || !validID.MatchString(c.ID) || !validID.MatchString(c.Order) || !validID.MatchString(c.Outlet) || c.Currency != p.Currency || c.Amount <= 0 || c.Amount > 1<<53-1 || (c.Kind != "refund" && (!p.Outlets[c.Outlet] || c.Amount > p.MaxAmount)) {
		return reject(422, "Command does not match the enrolled merchant program.")
	}
	if (c.Kind != "charge" && c.Kind != "refund") || (c.Kind == "charge" && c.Parent != "") || (c.Kind == "refund" && (!validID.MatchString(c.Parent) || c.Parent == c.ID)) {
		return reject(422, "Invalid operation or parent.")
	}
	if c.Channel == "hosted" && c.Terminal == "" {
		return nil
	}
	if c.Kind == "refund" && c.Channel == "terminal" && validID.MatchString(c.Terminal) {
		return nil
	}
	e, ok := s.terminals[c.Terminal]
	if c.Channel != "terminal" || !ok || e.Merchant != c.Merchant || e.Account != c.Account || e.Outlet != c.Outlet {
		return reject(422, "Terminal is not enrolled for this program and outlet.")
	}
	return nil
}

func (s *service) submit(ctx context.Context, c railcontract.Command) (railcontract.Fact, error) {
	var out railcontract.Fact
	if err := s.validate(c); err != nil {
		return out, err
	}
	err := s.ledgerTx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
		// Serialize the first use of this immutable operation ID; independent
		// checkouts and lanes do not share a financial write lock.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 84277016))", c.Account+"/"+c.ID); err != nil {
			return err
		}
		prior, err := s.scan(tx.QueryRow(ctx, "SELECT "+fields+" FROM virtualrail.operation WHERE "+original, c.Merchant, c.Account, c.ID))
		if err == nil {
			if prior.Command != c {
				return reject(409, "Original processor operation has different financial terms.")
			}
			out = prior
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		state := "pending"
		brand := ""
		var capability *string
		switch {
		case c.Kind == "refund":
			// Lock the original capture before checking all previously booked refunds.
			parent, err := s.scan(tx.QueryRow(ctx, "SELECT "+fields+" FROM virtualrail.operation WHERE "+original+" FOR UPDATE", c.Merchant, c.Account, c.Parent))
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			valid := err == nil && parent.Kind == "charge" && parent.State == "captured" && parent.Order == c.Order && parent.Outlet == c.Outlet && parent.Channel == c.Channel && parent.Terminal == c.Terminal && parent.Currency == c.Currency && sameRoute(parent.Command, c)
			state = "declined"
			if valid {
				var remaining int64
				if err := tx.QueryRow(ctx, `SELECT $4::bigint - coalesce(sum(amount),0) FROM virtualrail.operation WHERE merchant=$1 AND account=$2 AND parent=$3 AND kind='refund' AND state='refunded'`, c.Merchant, c.Account, c.Parent, parent.Amount).Scan(&remaining); err != nil {
					return err
				}
				if c.Amount <= remaining {
					state = "refunded"
					brand = parent.CardBrand
				}
			}
		case c.Channel == "terminal":
			// Take the lane lock, expire an abandoned presentation, then determine
			// whether this original can be presented. A busy lane records an
			// authoritative decline; retrying this operation cannot surprise-charge.
			if _, err := tx.Exec(ctx, "SELECT 1 FROM virtualrail.terminal WHERE id=$1 FOR UPDATE", c.Terminal); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE virtualrail.operation SET state='declined' WHERE terminal=$1 AND state='pending' AND deadline<=clock_timestamp()`, c.Terminal); err != nil {
				return err
			}
			var busy bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM virtualrail.operation WHERE terminal=$1 AND state='pending')", c.Terminal).Scan(&busy); err != nil {
				return err
			}
			if busy {
				state = "declined"
			}
		default:
			cap, err := randomID()
			if err != nil {
				return err
			}
			capability = &cap
		}
		ref := ""
		if state == "refunded" {
			ref, err = randomID()
			if err != nil {
				return err
			}
			ref = "mv_" + ref
		}
		out, err = s.scan(tx.QueryRow(ctx, `INSERT INTO virtualrail.operation(merchant,account,id,kind,parent,order_id,outlet,terminal,channel,amount,currency,state,movement,hosted_capability,payment_method,route_id,connector_id,provider_account,card_brand) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19) RETURNING `+fields, c.Merchant, c.Account, c.ID, c.Kind, c.Parent, c.Order, c.Outlet, c.Terminal, c.Channel, c.Amount, c.Currency, state, ref, capability, c.PaymentMethod, c.RouteID, c.ConnectorID, c.ProviderAccount, brand))
		if err != nil {
			return err
		}
		if state == "refunded" {
			return postMovement(ctx, tx, out, "refund", -c.Amount)
		}
		return nil
	})
	return out, err
}

func postMovement(ctx context.Context, tx pgx.Tx, f railcontract.Fact, kind string, amount int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO virtualrail.movement(reference,merchant,account,operation_id,kind,amount,currency) VALUES($1,$2,$3,$4,$5,$6,$7)`, f.Reference, f.Merchant, f.Account, f.ID, kind, amount, f.Currency)
	return err
}

// observe is the sole hosted/reader presentation state transition. Scope and
// decision race the deadline on the locked original row. A repeated approval of
// an already captured charge returns its fact with no extra financial movement.
func (s *service) observe(ctx context.Context, predicate string, args []any, decision, instrument string) (railcontract.Fact, error) {
	var f railcontract.Fact
	err := s.ledgerTx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		f, err = s.scan(tx.QueryRow(ctx, "SELECT "+fields+" FROM virtualrail.operation WHERE "+predicate+" FOR UPDATE", args...))
		if errors.Is(err, pgx.ErrNoRows) {
			return reject(404, "Presentation not found.")
		}
		if err != nil {
			return err
		}
		var brand string
		if decision == "approve" {
			brand, err = selectedBrand(f.PaymentMethod, instrument)
			if err != nil {
				return err
			}
			if f.State == "captured" && instrument != "" && f.CardBrand != brand {
				return reject(409, "Original presentation used a different instrument.")
			}
		}
		if f.State == "pending" {
			// The DB clock is sampled within the locked transition. A late
			// approval is a decline even when the HTTP request began before expiry.
			ref := ""
			if decision == "approve" {
				ref, err = randomID()
				if err != nil {
					return err
				}
				ref = "mv_" + ref
			}
			f, err = s.scan(tx.QueryRow(ctx, `WITH chosen AS MATERIALIZED (SELECT CASE
               WHEN deadline<=clock_timestamp() OR $4='decline' THEN 'declined'
               WHEN $4='approve' THEN 'captured' ELSE 'pending' END AS state
               FROM virtualrail.operation WHERE `+original+`)
               UPDATE virtualrail.operation o SET state=d.state, movement=CASE WHEN d.state='captured' THEN $5 ELSE '' END, card_brand=CASE WHEN d.state='captured' THEN $6 ELSE '' END
               FROM chosen d WHERE o.merchant=$1 AND o.account=$2 AND o.id=$3 AND d.state <> 'pending'
               RETURNING o.merchant,o.account,o.id,o.kind,o.parent,o.order_id,o.outlet,o.terminal,o.channel,o.amount,o.currency,o.state,o.movement,coalesce(o.hosted_capability,''),o.payment_method,o.route_id,o.connector_id,o.provider_account,o.card_brand`, f.Merchant, f.Account, f.ID, decision, ref, brand))
			if errors.Is(err, pgx.ErrNoRows) {
				f, err = s.scan(tx.QueryRow(ctx, "SELECT "+fields+" FROM virtualrail.operation WHERE "+predicate, args...))
			}
			if err != nil {
				return err
			}
			if f.State == "captured" {
				return postMovement(ctx, tx, f, "capture", f.Amount)
			}
		}
		return nil
	})
	if err == nil && ((decision == "approve" && f.State != "captured") || (decision == "decline" && f.State != "declined")) {
		return f, reject(409, "Original presentation already has a different outcome.")
	}
	return f, err
}

type absence struct {
	routeIdentity
	Code     string `json:"code"`
	Merchant string `json:"merchant_id"`
	Account  string `json:"processor_account"`
	ID       string `json:"operation_id"`
}

func (s *service) submitHTTP(w http.ResponseWriter, r *http.Request) {
	var c railcontract.Command
	if err := readBody(r, &c); err != nil {
		writeFailure(w, err)
		return
	}
	f, err := s.submit(r.Context(), c)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, 200, f)
}
func (s *service) lookupHTTP(w http.ResponseWriter, r *http.Request) {
	m, a, id := r.PathValue("merchant"), r.PathValue("account"), r.PathValue("id")
	p, ok := s.programs[a]
	if !ok || m != p.Merchant || !validID.MatchString(id) {
		writeJSON(w, 404, map[string]string{"error": "Unknown account or operation"})
		return
	}
	f, err := s.observe(r.Context(), original, []any{m, a, id}, "", "")
	var fail *failure
	if errors.As(err, &fail) && fail.code == 404 {
		writeJSON(w, 404, absence{routeIdentity{p.RouteID, p.ConnectorID, p.ProviderAccount}, "operation_absent", m, a, id})
		return
	}
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, 200, f)
}
func (s *service) hosted(ctx context.Context, cap, decision, brand string, walletScreen bool) (railcontract.Fact, error) {
	if !validCapability.MatchString(cap) {
		return railcontract.Fact{}, reject(404, "Presentation not found.")
	}
	predicate := "hosted_capability=$1 AND channel='hosted' AND kind='charge'"
	if walletScreen {
		predicate += " AND payment_method IN ('wechat_pay','alipay')"
	} else if decision != "" {
		predicate += " AND payment_method NOT IN ('wechat_pay','alipay')"
	}
	return s.observe(ctx, predicate, []any{cap}, decision, brand)
}

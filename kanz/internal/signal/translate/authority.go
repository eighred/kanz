package translate

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// FundAuthority answers the one question the signal path had never asked (#632):
// given the principal that was actually AUTHENTICATED, may it trade the fund it
// named, and whose book is that?
//
// # The defect it closes
//
// webhook-ingest is one of the two pods this repo permits on an Ingress. It
// authenticates the sender as a STRATEGY — an HMAC over the raw body against a
// per-strategy secret, which is exactly what Authenticator.Authenticate's own
// doc says it proves. It then took the TENANT from `fund_id` in the same body,
// through a seam that defaulted to the identity function when nobody wired it,
// and nobody ever did: no composition root in this module assigned it. So the
// tenant of a signal-originated order was the string the caller typed, and one
// holder of one strategy secret could publish SubmitOrder commands onto ANY
// tenant's book — from the public internet, with the audit trail attributing the
// trade to the victim.
//
// # Why this shape
//
// THE TENANT MUST NOT BE DERIVABLE FROM THE REQUEST. It is derived from two
// configured relations the caller cannot touch — strategy → funds, and fund →
// tenant — composed into a single lookup so no caller can perform one half and
// forget the other. A front end that resolved the tenant separately from the
// entitlement would be back where this started: two correct-looking checks with
// the gap between them.
//
// ok=false means REFUSE, and it does not say which half failed. An unknown fund
// and a fund this strategy has no claim on are the same answer to the caller:
// telling an unauthenticated-for-this-fund sender which fund ids exist is a
// probe oracle for the estate's account names.
//
// THERE IS NO DEFAULT IMPLEMENTATION and Options.Authority is required. The
// nil-defaults-to-identity seam this replaces is the precise anti-pattern that
// caused #632: a deployment that never configured the binding looked exactly
// like one that had, right up to the first cross-tenant order.
type FundAuthority interface {
	// TenantForFund returns the tenant that owns fundID, provided strategyID is
	// bound to it. ok=false means the signal is refused.
	TenantForFund(strategyID, fundID string) (tenant string, ok bool)
}

// StaticFundAuthority is the in-memory binding table, built from configuration at
// the composition root and immutable afterwards.
//
// Its constructor is where "nothing configured" and "checked, and fine" are made
// to look different: an empty table, a strategy with no funds, or a fund with no
// tenant are all STARTUP errors, so an unconfigured deployment cannot serve
// traffic at all rather than serving it into a tenant nobody chose.
type StaticFundAuthority struct {
	fundTenant map[string]string              // fund_id -> tenant_id
	bound      map[string]map[string]struct{} // strategy_id -> fund_ids
}

// NewFundAuthority builds the binding table and REFUSES anything that would let a
// signal through unbound.
//
// fundTenant maps every fund this deployment serves to the tenant that owns it.
// strategyFunds maps every strategy whose secret this deployment holds to the
// funds it may trade. Every fund named by a strategy must appear in fundTenant:
// a typo in an entitlement must not silently narrow to nothing, because a
// strategy that is entitled to a fund that does not exist is a strategy that
// stops trading with no error anywhere.
func NewFundAuthority(fundTenant map[string]string, strategyFunds map[string][]string) (*StaticFundAuthority, error) {
	if len(fundTenant) == 0 {
		return nil, fmt.Errorf("%w: no fund is bound to a tenant. Every fund this deployment serves "+
			"must declare the tenant that owns it — the tenant is NEVER taken from the request "+
			"body, which is authenticated as a strategy and says nothing about whose capital it is",
			ErrNoFundAuthority)
	}
	if len(strategyFunds) == 0 {
		return nil, fmt.Errorf("%w: no strategy is bound to a fund, so every authenticated signal "+
			"would be refused. Declare the funds each strategy may trade, or remove the strategy",
			ErrNoFundAuthority)
	}
	a := &StaticFundAuthority{
		fundTenant: make(map[string]string, len(fundTenant)),
		bound:      make(map[string]map[string]struct{}, len(strategyFunds)),
	}
	for fund, tenant := range fundTenant {
		if fund == "" {
			return nil, fmt.Errorf("%w: a fund with an empty id is declared", ErrNoFundAuthority)
		}
		if strings.TrimSpace(tenant) == "" {
			return nil, fmt.Errorf("%w: fund %q declares no tenant. An empty tenant publishes the "+
				"order onto the UNPREFIXED wire subject (see bus.TenantRoutedSubject), where the "+
				"platform account's own OMS picks it up — one fund's orders executed against "+
				"another's book, with nothing failing", ErrNoFundAuthority, fund)
		}
		a.fundTenant[fund] = tenant
	}
	for strategy, funds := range strategyFunds {
		if strategy == "" {
			return nil, fmt.Errorf("%w: a strategy with an empty id is declared", ErrNoFundAuthority)
		}
		if len(funds) == 0 {
			return nil, fmt.Errorf("%w: strategy %q is bound to no fund. That is a deny — it can "+
				"trade nothing — and this refuses to start rather than run a strategy whose secret "+
				"is live and whose every alert will be rejected", ErrNoFundAuthority, strategy)
		}
		set := make(map[string]struct{}, len(funds))
		for _, fund := range funds {
			if _, ok := a.fundTenant[fund]; !ok {
				return nil, fmt.Errorf("%w: strategy %q is bound to fund %q, which no `funds` entry "+
					"declares. Known funds: %s. A binding to a fund that does not exist takes that "+
					"strategy offline silently", ErrNoFundAuthority, strategy, fund, quotedKeys(a.fundTenant))
			}
			set[fund] = struct{}{}
		}
		a.bound[strategy] = set
	}
	return a, nil
}

// TenantForFund satisfies FundAuthority.
func (a *StaticFundAuthority) TenantForFund(strategyID, fundID string) (string, bool) {
	funds, ok := a.bound[strategyID]
	if !ok {
		return "", false
	}
	if _, ok := funds[fundID]; !ok {
		return "", false
	}
	tenant, ok := a.fundTenant[fundID]
	return tenant, ok
}

// Funds reports the funds strategyID may trade, sorted. For operator-facing
// diagnostics at startup — never for a response to a caller.
func (a *StaticFundAuthority) Funds(strategyID string) []string {
	out := make([]string, 0, len(a.bound[strategyID]))
	for f := range a.bound[strategyID] {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func quotedKeys(m map[string]string) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, `"`+k+`"`)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// ErrNoFundAuthority is a STARTUP failure: the strategy→fund→tenant binding table
// is absent or incoherent. It is deliberately not a per-signal error — a
// deployment that cannot say whose capital a strategy trades must not serve.
var ErrNoFundAuthority = errors.New("translate: invalid strategy/fund/tenant binding")

// ErrUnboundFund is returned when an authenticated strategy names a fund it is
// not bound to — the cross-tenant order injection of #632.
//
// It is its own sentinel rather than an ErrInvalidIntent: the intent is
// well-formed and the sender is authenticated. What was refused is the sender's
// AUTHORITY over the fund it named, and an operator seeing this in a log is
// looking at an attempted (or misconfigured) cross-tenant trade, not a malformed
// alert.
var ErrUnboundFund = errors.New("translate: strategy is not bound to that fund")

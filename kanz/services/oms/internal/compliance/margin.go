package compliance

import (
	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/venuemargin"
)

// MarginView is the OMS's local fold of the exchange's own margin observations —
// the half of #408 control 3 that control 1 already built. It is satisfied by
// *venuemargin.View.
//
// AN INTERFACE HERE ONLY SO THE ADAPTER IS TESTABLE WITHOUT A BUS. The shape is
// venuemargin.View's, deliberately, including the Quantity return: this package
// must not be able to obtain a margin ratio without the timestamp it was
// observed at, and a narrower seam returning a bare *big.Rat is exactly the
// separation venuemargin.Quantity has unexported fields to prevent.
type MarginView interface {
	MarginRatio(venue, account string) (venuemargin.Quantity, bool)
	Coverage(venue, account string) (venuemargin.Coverage, bool)
}

// MarginSource resolves a portfolio's margin state at a venue for the pre-trade
// gate: it turns (tenant, portfolio, venue) into the exchange ACCOUNT through
// the deploy-time bindings, then reads what the venue reported about it.
//
// # The two halves, and why they are joined here rather than in either package
//
// venuemargin publishes and folds per ACCOUNT, because a venue adapter holds one
// API credential and therefore IS one exchange account; it does not know which
// portfolio is bound to it. execution.AccountBindings holds that mapping, and it
// is OMS deploy-time configuration. Neither package can do this join without
// depending on the other's concern, so the join lives at the consumer — this
// adapter — which is the same place the OMS already resolves an account to route
// an order (order.Service.resolveAccount).
//
// # The resolution is SINGLE-VALUED, and that is a control and not a convenience
//
// ParseBindings refuses a binding set where two portfolios share one account, and
// the OMS exits 2 rather than start on one (#415, control 2). That is what makes
// "this portfolio's margin state" a well-formed question: on a shared account it
// would not be, because the exchange would liquidate one portfolio's position out
// of another's collateral and no per-portfolio answer could be true.
//
// # An UNBOUND portfolio is UNKNOWN, not exempt
//
// With no binding the OMS may still route the order — against whatever account
// the adapter holds, alongside every other unbound portfolio (see
// Service.noteShared). That is a shared collateral pool, so there is no account
// whose margin can be said to back THIS portfolio, and the honest answer is
// unknown. The rule refuses on it. A deployment that declares margin trading
// must bind its accounts; that is control 2 being load-bearing rather than
// merely prudent, which is precisely what #408 says happens the moment margin is
// switched on.
type MarginSource struct {
	bindings *execution.AccountBindings
	view     MarginView
}

// NewMarginSource wires the adapter.
//
// A nil view or nil bindings yields a source that answers UNKNOWN to everything,
// rather than a nil source that the gate would read as "no margin control
// configured". Both refuse; the difference is that this one keeps the seam
// present so a misconfiguration cannot arrive as an absence.
func NewMarginSource(bindings *execution.AccountBindings, view MarginView) *MarginSource {
	return &MarginSource{bindings: bindings, view: view}
}

var _ comp.MarginSource = (*MarginSource)(nil)

// Margin implements comp.MarginSource. ok=false is UNKNOWN and the caller must
// refuse: no view, no binding, never observed, observed too long ago, or
// observed without a margin ratio all arrive here as the same answer, which is
// the answer the rule fails closed on.
func (s *MarginSource) Margin(tenantID, portfolioID, venue string) (comp.MarginState, bool) {
	if s == nil || s.view == nil || venue == "" {
		return comp.MarginState{}, false
	}
	account, bound := s.bindings.Account(tenantID, portfolioID, venue)
	if !bound {
		return comp.MarginState{}, false
	}
	// COVERAGE FIRST, AND FROM THE SAME OBSERVATION. Both lookups apply the view's
	// freshness bound to the one snapshot this account holds, so they cannot be
	// answered from different polls — a completeness check satisfied by one
	// observation and a number taken from another would be the stale book with an
	// extra step.
	cov, ok := s.view.Coverage(venue, account)
	if !ok {
		return comp.MarginState{}, false
	}
	st := comp.MarginState{
		Account:          account,
		CoverageReported: cov.Reported(),
		ExcludedCount:    cov.ExcludedCount(),
	}
	// THE RATIO ARRIVES WITH ITS TIMESTAMP AND IS CARRIED WITH IT. venuemargin
	// makes that structural — Quantity has no accessor yielding the number alone —
	// and this is the seam where it would otherwise be dropped, so ObservedAt is
	// filled from the same value the ratio came out of.
	if q, held := s.view.MarginRatio(venue, account); held {
		st.Ratio = q.Value()
		st.ObservedAt = q.ObservedAt()
	}
	// ok=true means "we have a CURRENT observation of this account". Whether that
	// observation is usable — complete, and carrying a ratio — is the rule's
	// decision, made against the evidence above, because the rule is what has to
	// tell an operator which of those states it refused on.
	return st, true
}

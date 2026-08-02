// Package compliance is the OMS's pre-trade gate seam (OMS-01f). The order
// handler calls Check before admitting an order; a non-nil *Breach refuses it
// (deny-by-default once a real gate is wired). The default AllowAll gate keeps
// the OMS runnable before COMP-01 exists — the COMP-01 engine implements this
// same interface and is injected at the composition root (the repo's
// inject-the-side-effect stance), so the handler never changes.
package compliance

import (
	"context"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// Breach is a refused pre-trade check: a stable Code (e.g. "CONCENTRATION",
// "RESTRICTED_INSTRUMENT") and a human-readable Reason. The handler maps it to
// an ORDER_REJECTED FACT + a REJECTED command outcome.
type Breach struct {
	Code   string
	Reason string
}

// Gate decides whether a candidate order may be admitted. Check returns nil to
// allow; a non-nil *Breach to reject. The candidate is the SubmitOrder so the
// real gate (COMP-01) can project the post-trade book and run the portfolio's
// mandate rules.
//
// tenantID is WHOSE order it is, off the command envelope — SubmitOrder itself
// carries no tenant, and portfolio_id is a caller-chosen string, so without this
// parameter the mandate lookup had nothing but "growth" to go on and tenant B's
// order was cleared against tenant A's limits (#243). It is a parameter rather
// than something the adapter reads off its own config precisely so that every
// implementation is forced to be given one; the OMS's own configured tenant is
// __system__ and is the wrong answer here.
type Gate interface {
	Check(ctx context.Context, tenantID string, cmd *orderpb.SubmitOrder) (*Breach, error)
}

// AllowAll admits every order. The placeholder until COMP-01 is wired.
type AllowAll struct{}

// Check always allows.
func (AllowAll) Check(context.Context, string, *orderpb.SubmitOrder) (*Breach, error) {
	return nil, nil
}

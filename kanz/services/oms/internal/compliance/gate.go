// Package compliance is the OMS's pre-trade gate seam (OMS-01f). The order
// handler calls Check before admitting an order; a non-nil *Breach refuses it
// (deny-by-default once a real gate is wired). The default AllowAll gate keeps
// the OMS runnable before COMP-01 exists — the COMP-01 engine implements this
// same interface and is injected at the composition root (the repo's
// inject-the-side-effect stance), so the handler never changes.
package compliance

import (
	"context"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
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
type Gate interface {
	Check(ctx context.Context, cmd *orderpb.SubmitOrder) (*Breach, error)
}

// AllowAll admits every order. The placeholder until COMP-01 is wired.
type AllowAll struct{}

// Check always allows.
func (AllowAll) Check(context.Context, *orderpb.SubmitOrder) (*Breach, error) { return nil, nil }

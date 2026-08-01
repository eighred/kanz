package credit

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/eighred/kanz/internal/risk/xva"
)

// QuoteSource supplies the CDS quote set for a reference entity as of a point in
// time — the calibrator-local seam a live quote surface adapts to at the
// composition root, exactly as curve.QuoteSource and volsurface.QuoteProvider do.
//
// Recovery comes back WITH the quotes rather than being configured on the
// Calibrator, because it is a property of the quoted contract and not of this
// process: a par spread means nothing without the recovery it was quoted at, and
// pairing a fresh spread with a stale recovery silently reprices the curve. It is
// the same reasoning that has volsurface return spot alongside its quotes.
type QuoteSource interface {
	CDSQuotes(ctx context.Context, reference string, asOf time.Time) (quotes []xva.CDSQuote, recovery float64, err error)
}

// Calibrator ties the seam together: pull quotes, bootstrap, publish the curve
// point-in-time. The composition root drives Refresh on its schedule; the Store
// then resolves the curve at any as_of for the XVA layer.
type Calibrator struct {
	Source QuoteSource
	Store  *Store
	// Disc discounts the premium and protection legs. Nil means UNDISCOUNTED
	// (DF ≡ 1), which xva.BootstrapCDS accepts — see Refresh for why that is a
	// deliberate posture rather than a default worth hiding.
	Disc DiscountSource
}

// DiscountSource is the risk-free discounting the bootstrap prices against. It is
// this narrow rather than pricing.DiscountCurve so the rates calibrator's own
// curve store can satisfy it directly at the composition root without either
// package importing the other.
type DiscountSource interface {
	// Discount returns DF(t) for t years.
	Discount(t float64) float64
}

// FlatRate is a constant continuously-compounded discounting rate, for a
// deployment that has no calibrated rate curve yet. It is a TYPE the caller must
// name rather than a default on Calibrator: discounting at a made-up flat rate
// is a choice about what the CVA number means, and it should appear in the
// composition root where somebody can see it.
type FlatRate float64

// Discount implements DiscountSource.
func (r FlatRate) Discount(t float64) float64 {
	if t <= 0 {
		return 1
	}
	return math.Exp(-float64(r) * t)
}

// Refresh bootstraps the reference entity's credit curve from CDS quotes as of
// asOf and publishes it to the store, returning the calibrated curve.
//
// A source error or an unbootstrappable quote set LEAVES THE STORE UNCHANGED —
// the previous curve keeps serving rather than being replaced by nothing. That
// is the same contract curve.Refresh and volsurface.Refresh state, and it is the
// one that matters most here: xva integrates CVA against Survival(t), so a curve
// silently replaced by a zero-hazard one would report a counterparty that cannot
// default, and CVA would fall to zero without anything failing.
func (cal *Calibrator) Refresh(ctx context.Context, reference string, asOf time.Time) (*xva.CreditCurve, error) {
	quotes, recovery, err := cal.Source.CDSQuotes(ctx, reference, asOf)
	if err != nil {
		return nil, fmt.Errorf("credit: quote source for %s: %w", reference, err)
	}

	// nil df means undiscounted in xva.BootstrapCDS. Passing it through rather
	// than substituting a flat rate keeps the choice visible: an undiscounted
	// bootstrap is a real posture for short tenors, and inventing a rate here
	// would put a number nobody chose inside a curve the XVA layer trusts.
	var df func(t float64) float64
	if cal.Disc != nil {
		df = cal.Disc.Discount
	}

	c, err := xva.BootstrapCDS(quotes, recovery, df)
	if err != nil {
		return nil, fmt.Errorf("credit: bootstrap %s: %w", reference, err)
	}
	cal.Store.Put(reference, asOf, &c)
	return &c, nil
}

// RefreshFunc adapts Refresh to the generic scheduler's signature, so a
// composition root can add credit to the same Scheduler that drives curve and
// vol rather than growing a second timer for it.
//
// The reference entity is bound here rather than passed at tick time because a
// schedule.Job is one calibration target on one cadence — the same shape the
// rates and vol jobs take.
func (cal *Calibrator) RefreshFunc(reference string) func(ctx context.Context, asOf time.Time) error {
	return func(ctx context.Context, asOf time.Time) error {
		_, err := cal.Refresh(ctx, reference, asOf)
		return err
	}
}

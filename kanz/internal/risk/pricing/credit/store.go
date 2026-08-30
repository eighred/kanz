// Package credit is the calibration seam for counterparty credit curves — the
// third member of the family, beside internal/risk/pricing/curve (rates) and
// internal/risk/pricing/volsurface (vol).
//
// # What was missing, and what was not (#113)
//
// The credit MODEL was already here and is not reimplemented: xva.BootstrapCDS
// solves the piecewise-constant forward hazards so every quoted CDS reprices to
// par, and xva.CreditCurve carries them with Survival(t). What did not exist was
// the SEAM the other two have — a quote source, a point-in-time store, and a
// Refresh the generic scheduler can drive. So credit calibration could be
// computed but not SCHEDULED, which is the gap #113 names.
//
// Everything here mirrors curve and volsurface deliberately, down to the method
// names. Three calibrators that each invented their own shape would be three
// things to learn and three places for the point-in-time semantics to drift; the
// one thing worth having in common is exactly the thing a reader has to trust.
package credit

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing/pit"
	"github.com/eighred/kanz/internal/risk/xva"
)

// Store holds calibrated credit curves point-in-time, keyed by reference entity.
//
// Point-in-time rather than latest-only for the same reason curve.Store is: a
// risk number is reproduced AS OF a date, and a store that only knows "now"
// cannot answer what the book was worth last Tuesday — which is the question an
// auditor asks.
//
// Retention is bounded by a horizon (#811), through the same pit.Put every
// member of this family uses. The mirror is the point, here as everywhere else
// in this file: a horizon added to one of the three stores and not the others
// is the copied-helper failure mode, and the horizon is a financial decision
// that must be one decision rather than three.
type Store struct {
	mu          sync.RWMutex
	byReference map[string][]pit.Version[*xva.CreditCurve] // ascending by AsOf
	horizon     time.Duration
}

// Option configures a Store.
type Option func(*Store)

// WithHorizon sets how far back versions are retained, measured from the newest
// version held for that reference entity. Zero or negative selects
// pit.DefaultHorizon; there is no "retain forever".
func WithHorizon(d time.Duration) Option {
	return func(s *Store) { s.horizon = pit.Horizon(d) }
}

// NewStore returns an empty point-in-time store retaining pit.DefaultHorizon of
// versions unless WithHorizon says otherwise.
func NewStore(opts ...Option) *Store {
	s := &Store{byReference: map[string][]pit.Version[*xva.CreditCurve]{}, horizon: pit.DefaultHorizon}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Put publishes a curve for the reference entity effective at asOf. Versions may
// arrive out of order (a nightly-close backfill behind an intraday refresh); the
// version list stays sorted. A second Put at the same asOf replaces. Versions
// older than the horizon are dropped here, under the write lock already held.
func (s *Store) Put(reference string, asOf time.Time, c *xva.CreditCurve) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs, _ := pit.Put(s.byReference[reference], asOf, c, s.horizon)
	s.byReference[reference] = vs
}

// Curve resolves the latest curve effective at or before asOf. ok=false when the
// reference has no curve yet or asOf predates the first version retained.
//
// FALSE IS NOT A ZERO CURVE. A zero-hazard curve means "this counterparty cannot
// default", which prices CVA at zero — so a missing curve must be reported as
// missing and refused by the caller, never substituted. This is the same stance
// mark.Source takes for a missing price.
func (s *Store) Curve(_ context.Context, reference string, asOf time.Time) (*xva.CreditCurve, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	vs := s.byReference[reference]
	return pit.At(vs, asOf)
}

// References lists every reference entity with at least one calibrated curve,
// sorted. The composition root uses it to build one scheduler Job per entity.
func (s *Store) References() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.byReference))
	for r := range s.byReference {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

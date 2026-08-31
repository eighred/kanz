package curve

import (
	"context"
	"sync"
	"time"

	"github.com/eighred/kanz/internal/pit"
)

// Store is the point-in-time curve store behind the FI-01d CurveProvider seam
// (PARITY-03a): each Refresh publishes a calibrated curve versioned by its
// as-of, and a read resolves the latest curve effective at the requested as_of
// — so a risk run at T prices off the curve that was live at T, never a later
// one (the MODEL-01b point-in-time stance). Satisfies compute.CurveProvider
// implicitly; the compile-time assertion lives in the compute package (this
// package cannot import compute).
//
// Retention is bounded by a horizon (#811) and the mechanism is pit.Put, shared
// with credit.Store and volsurface.Store — three copies of a prune would be the
// copied-helper failure mode, and the horizon is the kind of decision that must
// be one decision. Read pit's package doc for why a horizon does not cost audit
// evidence here: the store is in-memory and starts empty, so no read has ever
// been able to reach past the process start.
type Store struct {
	mu         sync.RWMutex
	byCurrency map[string][]pit.Version[*Curve] // ascending by AsOf
	horizon    time.Duration
}

// Option configures a Store.
type Option func(*Store)

// WithHorizon sets how far back versions are retained, measured from the newest
// version held for that currency. Zero or negative selects pit.DefaultHorizon —
// there is deliberately no way to configure "retain forever", because that is
// the state #811 found and it reported itself as healthy the whole time.
func WithHorizon(d time.Duration) Option {
	return func(s *Store) { s.horizon = pit.Horizon(d) }
}

// NewStore returns an empty point-in-time store retaining pit.DefaultHorizon of
// versions unless WithHorizon says otherwise.
func NewStore(opts ...Option) *Store {
	s := &Store{byCurrency: map[string][]pit.Version[*Curve]{}, horizon: pit.DefaultHorizon}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Put publishes a curve for the currency effective at asOf. Versions may
// arrive out of order (a nightly-close backfill behind an intraday refresh);
// the version list stays sorted. A second Put at the same asOf replaces.
// Versions older than the horizon are dropped here, under the write lock that
// is already held.
func (s *Store) Put(currency string, asOf time.Time, c *Curve) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs, _ := pit.Put(s.byCurrency[currency], asOf, c, s.horizon)
	s.byCurrency[currency] = vs
}

// Curve resolves the latest curve effective at or before asOf — the
// compute.CurveProvider contract. ok=false when the currency has no curve yet
// or asOf predates the first version retained.
func (s *Store) Curve(_ context.Context, currency string, asOf time.Time) (*Curve, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	vs := s.byCurrency[currency]
	return pit.At(vs, asOf)
}

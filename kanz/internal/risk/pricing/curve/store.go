package curve

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Store is the point-in-time curve store behind the FI-01d CurveProvider seam
// (PARITY-03a): each Refresh publishes a calibrated curve versioned by its
// as-of, and a read resolves the latest curve effective at the requested as_of
// — so a risk run at T prices off the curve that was live at T, never a later
// one (the MODEL-01b point-in-time stance). Satisfies compute.CurveProvider
// implicitly; the compile-time assertion lives in the compute package (this
// package cannot import compute).
type Store struct {
	mu         sync.RWMutex
	byCurrency map[string][]curveVersion // ascending by asOf
}

type curveVersion struct {
	asOf time.Time
	c    *Curve
}

// NewStore returns an empty point-in-time store.
func NewStore() *Store {
	return &Store{byCurrency: map[string][]curveVersion{}}
}

// Put publishes a curve for the currency effective at asOf. Versions may
// arrive out of order (a nightly-close backfill behind an intraday refresh);
// the version list stays sorted. A second Put at the same asOf replaces.
func (s *Store) Put(currency string, asOf time.Time, c *Curve) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs := s.byCurrency[currency]
	i := sort.Search(len(vs), func(i int) bool { return !vs[i].asOf.Before(asOf) })
	if i < len(vs) && vs[i].asOf.Equal(asOf) {
		vs[i].c = c
	} else {
		vs = append(vs, curveVersion{})
		copy(vs[i+1:], vs[i:])
		vs[i] = curveVersion{asOf: asOf, c: c}
	}
	s.byCurrency[currency] = vs
}

// Curve resolves the latest curve effective at or before asOf — the
// compute.CurveProvider contract. ok=false when the currency has no curve yet
// or asOf predates the first version.
func (s *Store) Curve(_ context.Context, currency string, asOf time.Time) (*Curve, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	vs := s.byCurrency[currency]
	i := sort.Search(len(vs), func(i int) bool { return vs[i].asOf.After(asOf) })
	if i == 0 {
		return nil, false
	}
	return vs[i-1].c, true
}

package compliance

import (
	"context"
	"sort"
	"sync"
	"time"

	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
)

// MandateRegistry is an in-memory, point-in-time store of mandate versions per
// portfolio — the COMP-01f resolution core. Each mandate change is appended as
// a new version with its effective_at; resolution returns the version in effect
// at a query time (the latest version whose effective_at ≤ asOf). The compliance
// service loads it by replaying the lifecycle.v1.ConfigChanged FACTs that carry
// each serialized Mandate, so "which mandate applied when" is reconstructable.
//
// It satisfies MandateSource, so the pre-trade gate resolves through it directly.
type MandateRegistry struct {
	mu sync.RWMutex
	// byPortfolio holds versions sorted ascending by (effective_at, version).
	byPortfolio map[string][]*compliancepb.Mandate
}

// NewMandateRegistry returns an empty registry.
func NewMandateRegistry() *MandateRegistry {
	return &MandateRegistry{byPortfolio: make(map[string][]*compliancepb.Mandate)}
}

// Put inserts a mandate version, keeping the portfolio's history sorted by
// (effective_at, version). Re-putting the same version replaces it (idempotent
// replay).
func (r *MandateRegistry) Put(m *compliancepb.Mandate) {
	if m == nil || m.GetPortfolioId() == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pid := m.GetPortfolioId()
	vers := r.byPortfolio[pid]
	for i, v := range vers {
		if v.GetVersion() == m.GetVersion() {
			vers[i] = m // idempotent replace
			r.byPortfolio[pid] = vers
			return
		}
	}
	vers = append(vers, m)
	sort.Slice(vers, func(i, j int) bool {
		ti, tj := vers[i].GetEffectiveAt().AsTime(), vers[j].GetEffectiveAt().AsTime()
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return vers[i].GetVersion() < vers[j].GetVersion()
	})
	r.byPortfolio[pid] = vers
}

// Mandate resolves the mandate version in effect for portfolioID at asOf. A
// zero asOf resolves to the latest version. ok=false when no version is in
// effect (the portfolio has no mandate at that time), which the gate treats as
// "no constraints".
func (r *MandateRegistry) Mandate(_ context.Context, portfolioID string, asOf time.Time) (*compliancepb.Mandate, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	vers := r.byPortfolio[portfolioID]
	if len(vers) == 0 {
		return nil, false, nil
	}
	var chosen *compliancepb.Mandate
	for _, v := range vers {
		eff := v.GetEffectiveAt().AsTime()
		if asOf.IsZero() || !eff.After(asOf) {
			chosen = v // versions are ascending, so the last match wins
		}
	}
	if chosen == nil {
		return nil, false, nil
	}
	return chosen, true, nil
}

var _ MandateSource = (*MandateRegistry)(nil)

// MapBookSource is an in-memory BookSource keyed by portfolio id — the
// composition root populates it from the position read model; tests construct it
// directly.
type MapBookSource map[string]*Book

// Book implements BookSource. A missing portfolio returns an empty book (no
// holdings) rather than an error, so an order for a never-traded portfolio is
// evaluated against an empty book rather than failing transiently.
func (m MapBookSource) Book(_ context.Context, portfolioID string) (*Book, error) {
	if b, ok := m[portfolioID]; ok {
		return b, nil
	}
	return &Book{PortfolioID: portfolioID}, nil
}

var _ BookSource = MapBookSource(nil)

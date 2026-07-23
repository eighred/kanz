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
	armed       bool
	onceOn      sync.Once
}

// NewMandateRegistry returns an empty, UNARMED registry.
func NewMandateRegistry() *MandateRegistry {
	return &MandateRegistry{byPortfolio: make(map[string][]*compliancepb.Mandate)}
}

// Arm marks the initial mandate replay complete — mirrors PositionCache.Arm
// (services/webhook-ingest/internal/ingest/positions.go), the pattern this
// copies rather than invents.
//
// Both the OMS's pre-trade gate and compliance's post-trade monitor used to call
// readiness.Set(true) the instant the mandate subscription GOROUTINE LAUNCHED,
// not once it had actually folded the mandates in force. Between "pod reports
// Ready" and "the replay has landed" every portfolio resolved as having no
// mandate at all — and OMS_REQUIRE_MANDATE defaults to false, so a portfolio
// with no mandate is not refused, it is ADMITTED UNCONSTRAINED (see the OMS's
// own startup warning). That window is exactly EXEC-M13 — "a restarted OMS
// came back with an empty registry and its gate passed every order" — except
// widened from "only on a broken durable-group replay" to "on every single
// rolling restart, for however long the fold takes". The control did not fail
// loudly; it disarmed silently while the health check said everything was
// fine.
//
// Gating readiness on Arm having run closes that window: the pod stays out of
// its Service (kubelet-wise) or the process withholds readiness.Set(true)
// until the registry can truthfully answer "no mandate" instead of merely
// "no mandate YET". sync.Once-guarded and idempotent, same as PositionCache —
// the bus may call it more than once across resubscribes, and a second call
// must not reopen the question of whether the initial replay landed.
func (r *MandateRegistry) Arm() {
	r.onceOn.Do(func() {
		r.mu.Lock()
		r.armed = true
		r.mu.Unlock()
	})
}

// Armed reports whether the initial mandate replay has folded. Both mains gate
// readiness.Set(true) on this: a pod that has not yet learned which mandates
// are in force must not be handed orders to admit against a registry that
// looks empty but is merely not caught up yet (EXEC-M13).
func (r *MandateRegistry) Armed() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.armed
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

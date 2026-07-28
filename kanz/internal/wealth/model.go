package wealth

import (
	"math"
	"sort"
	"time"

	"github.com/eighred/kanz/internal/optimization"
)

// Model portfolios + proposals (WEALTH-01d): map a household's risk profile to a
// model portfolio, measure how far the household's actual book has drifted from
// that model, and — when the drift breaches a band — produce a rebalancing
// proposal. The proposal is built by REUSING the OPT-01 rebalance engine
// (internal/optimization, a peer outside the risk module), so the advisor and the
// PM share one trade-list engine; the target it rebalances toward is the model
// for the household's profile, so the proposal always respects the risk profile.

// RiskProfile is a household's risk tolerance band — mirrors
// wealth.v1.RiskProfile. It selects exactly one ModelPortfolio.
type RiskProfile int

const (
	// ProfileUnspecified is the zero value; never select on it.
	ProfileUnspecified RiskProfile = iota
	ProfileConservative
	ProfileModerate
	ProfileBalanced
	ProfileGrowth
	ProfileAggressive
)

// ModelPortfolio is the target allocation for a risk profile — the book a
// household of that profile should hold. Targets maps instrument id to its target
// weight; the weights are expected to sum to 1.
type ModelPortfolio struct {
	ModelID string
	Profile RiskProfile
	Targets map[string]float64
}

// SelectModel returns the model serving profile, or false if none does. Models
// are keyed one-per-profile (the WEALTH-01d invariant); the first match wins, so
// a well-formed catalog has at most one model per profile.
func SelectModel(profile RiskProfile, models []ModelPortfolio) (ModelPortfolio, bool) {
	for _, m := range models {
		if m.Profile == profile {
			return m, true
		}
	}
	return ModelPortfolio{}, false
}

// Drift is the per-instrument deviation of a household's actual weights from its
// model. Positive = overweight vs the model, negative = underweight. The union of
// held and model instruments is covered (a held name absent from the model drifts
// by its full weight; a model name not held drifts by its full target).
type Drift struct {
	// ByInstrument is instrument id → (actual − target) weight.
	ByInstrument map[string]float64
	// Max is the largest absolute per-instrument drift.
	Max float64
	// Total is Σ|drift| — the aggregate one-way distance from the model.
	Total float64
}

// ComputeDrift diffs a household's actual weights against a model's targets.
func ComputeDrift(actual map[string]float64, model ModelPortfolio) Drift {
	d := Drift{ByInstrument: map[string]float64{}}
	seen := map[string]struct{}{}
	for id, w := range actual {
		dw := w - model.Targets[id]
		d.ByInstrument[id] = dw
		seen[id] = struct{}{}
	}
	for id, tw := range model.Targets {
		if _, ok := seen[id]; ok {
			continue
		}
		d.ByInstrument[id] = -tw // held weight is 0
	}
	for _, dw := range d.ByInstrument {
		a := math.Abs(dw)
		if a > d.Max {
			d.Max = a
		}
		d.Total += a
	}
	return d
}

// Breached reports whether the household has drifted past tolerance on any single
// instrument — the trigger for a rebalancing proposal (no-trade-while-in-band, the
// minimality discipline OPT-01 shares).
func (d Drift) Breached(tolerance float64) bool {
	return d.Max > tolerance
}

// Proposal is a household rebalancing recommendation — the model it should track,
// the drift that triggered it, and the minimal trade list to get there. It wraps
// the OPT-01 RebalanceProposal so the advisor surface carries the same trade
// shape the PM workflow does.
type Proposal struct {
	HouseholdID string
	ModelID     string
	Profile     RiskProfile
	Drift       Drift
	Rebalance   optimization.RebalanceProposal
}

// Propose builds a rebalancing proposal that moves a household's virtual
// portfolio onto the model for its profile. It selects the model, computes the
// drift, and — reusing optimization.Rebalance — diffs the household's current
// weights against the model targets into a minimal trade list. The proposal's
// targets ARE the selected model's weights, so it respects the risk profile by
// construction. Returns false if no model serves the profile.
func Propose(profile RiskProfile, models []ModelPortfolio, vp VirtualPortfolio, prices map[string]float64, threshold float64, asOf time.Time) (Proposal, bool) {
	model, ok := SelectModel(profile, models)
	if !ok {
		return Proposal{}, false
	}
	current := vp.Weights()
	drift := ComputeDrift(current, model)
	rb := optimization.Rebalance(vp.HouseholdID, current, model.Targets, vp.TotalValue, prices, threshold, asOf)
	return Proposal{
		HouseholdID: vp.HouseholdID,
		ModelID:     model.ModelID,
		Profile:     profile,
		Drift:       drift,
		Rebalance:   rb,
	}, true
}

// TargetInstruments returns the model's instrument ids in deterministic order.
func (m ModelPortfolio) TargetInstruments() []string {
	out := make([]string, 0, len(m.Targets))
	for id := range m.Targets {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

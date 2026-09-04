package wealth

import (
	"errors"
	"fmt"
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

// String names the profile for a log line, an operator-facing JSON field and an
// error message. It exists because the alternative everywhere is the raw ordinal:
// "profile=4" in a warning about a household nobody is checking is a number an
// operator has to go look up, and a read surface that returns 0 for
// ProfileUnspecified is indistinguishable from one that omitted the field.
func (p RiskProfile) String() string {
	switch p {
	case ProfileConservative:
		return "CONSERVATIVE"
	case ProfileModerate:
		return "MODERATE"
	case ProfileBalanced:
		return "BALANCED"
	case ProfileGrowth:
		return "GROWTH"
	case ProfileAggressive:
		return "AGGRESSIVE"
	case ProfileUnspecified:
		return "UNSPECIFIED"
	default:
		// Named rather than rendered blank: a profile this build does not know
		// reaching a log line is itself the finding.
		return fmt.Sprintf("RiskProfile(%d)", int(p))
	}
}

// ModelPortfolio is the target allocation for a risk profile — the book a
// household of that profile should hold. Targets maps instrument id to its target
// weight; the weights are expected to sum to 1.
type ModelPortfolio struct {
	ModelID string
	Profile RiskProfile
	Targets map[string]float64
	// Tolerance is the per-instrument weight band this model may drift by before
	// a rebalance is due — the argument Drift.Breached is compared against. It
	// lives BESIDE THE MODEL rather than in the evaluator because it is a policy
	// choice that differs per model, and a band compiled into code cannot be
	// changed without a deploy.
	//
	// A zero tolerance is REFUSED by Validate, not defaulted: Breached compares
	// `max > tolerance`, so a zero band marks every household holding anything at
	// all as permanently breached, and "nobody set a band" would be
	// indistinguishable from "rebalance on any deviation whatsoever".
	Tolerance float64
	// RecordedBy and Reason are the operator principal and the decision this model
	// records — carried into the domain rather than dropped at the decode because
	// the registry's refusals name them. Two models claiming one risk profile is
	// an operator error somebody has to fix, and "growth-2024 (operator:akif) and
	// growth-2025 (operator:sam) both claim GROWTH" is the difference between a
	// log line an operator can act on and one they have to go digging behind.
	RecordedBy string
	Reason     string
}

// weightSumTolerance is how far Σ targets may sit from 1 before Validate refuses
// the model. The weights arrive as operator-authored protojson decimals, so an
// exact comparison would reject a correct model for the last bit of a base-2
// rounding; 1e-6 is far tighter than any allocation anybody writes by hand and
// far looser than float64 noise over a few dozen instruments.
const weightSumTolerance = 1e-6

// Validate reports why a model portfolio may not be admitted to the catalogue,
// or nil. It is called in TWO places on purpose — by cmd/kanz-model before it
// dials the broker, and by ModelRegistry.Put on delivery — because the model
// subject is COMPACTED: a bad publish is not recoverable by leaving it alone, it
// stays the answer for that model id until somebody publishes another. Validating
// only at the consumer would let a wrong model become every consumer's permanent
// state; validating only at the publisher would trust that this tool is the only
// writer the subject will ever have.
//
// Every rule here exists because breaking it produces a CONFIDENTLY WRONG drift
// rather than an error. A model whose weights sum to 0.5 reports every household
// of that profile as ~50 points overweight; a model with no targets reports every
// held instrument as fully drifted; an unspecified profile serves no household at
// all while looking like a published model.
func (m ModelPortfolio) Validate() error {
	switch {
	case m.ModelID == "":
		return errors.New("model_id is required: it is the catalogue key and the subject's compaction key")
	case m.Profile == ProfileUnspecified:
		return errors.New("risk_profile is required: it is the ONLY thing that selects this model for a household, " +
			"and an unspecified profile serves nobody while looking like a published model")
	case len(m.Targets) == 0:
		return errors.New("target_weights is required: a model with no targets reports every instrument a household " +
			"holds as fully drifted from it")
	case m.Tolerance <= 0:
		return errors.New("drift_tolerance must be > 0: Breached compares max > tolerance, so a zero band marks every " +
			"household holding anything at all as permanently breached")
	case m.Tolerance > 1:
		return fmt.Errorf("drift_tolerance %v exceeds 1: a band wider than the whole book can never be breached, "+
			"which silently disables rebalancing for every household on this model", m.Tolerance)
	case m.RecordedBy == "":
		return errors.New("recorded_by is required: this subject is compacted, so publishing a model discards the " +
			"one it replaced and nothing is left to attribute the change to")
	case m.Reason == "":
		return errors.New("reason is required: an unexplained overwrite of the allocation every household of a risk " +
			"profile is measured against is not auditable")
	}
	var sum float64
	for _, w := range m.Targets {
		sum += w
	}
	if math.Abs(sum-1) > weightSumTolerance {
		return fmt.Errorf("target_weights sum to %v, not 1: every household on this model would be reported as "+
			"%v of drift that is the model's arithmetic, not the book's", sum, math.Abs(sum-1))
	}
	return nil
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

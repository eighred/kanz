package optimization

import (
	"github.com/eighred/kanz/internal/dec"
	"math"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

// Constraint engine (OPT-01c): the optimizer's feasible region, plus the
// reuse of the COMP-01 mandate rules as optimization constraints — one source of
// truth, "a book you can't hold, you can't optimize into". Two mechanisms:
//
//  1. Box constraints the solver enforces directly (long-only, per-instrument
//     and global weight bounds) — derived from a mandate's INSTRUMENT-dimension
//     concentration limits and restriction lists where they map cleanly to a
//     box.
//  2. A full post-optimization feasibility CHECK that projects the target
//     weights into a compliance.Book and runs the COMP-01 engine, catching every
//     rule (sector/issuer concentration, leverage, currency) including those a
//     weight box cannot express — deny-by-default, the exact engine the pre-trade
//     gate uses.

// ConstraintSet is the optimizer's feasible region (Go-native shape of
// optimization.v1.ConstraintSet).
type ConstraintSet struct {
	LongOnly    bool
	MinWeight   float64 // global per-instrument floor (when no per-instrument bound)
	MaxWeight   float64 // global per-instrument cap; 0 ⇒ no cap (treated as 1)
	Bounds      map[string]WeightBound
	SectorCaps  map[string]float64 // sector bucket → max aggregate weight
	MaxTurnover float64
}

// WeightBound is a per-instrument box.
type WeightBound struct {
	Min, Max float64
}

// bounds builds the lo/hi vectors the solver's projection clamps to, aligned to
// instruments. The default (nil cons) is long-only, fully invested, unbounded
// above (lo=0, hi=1). A per-instrument bound overrides the global min/max.
func bounds(instruments []string, cons *ConstraintSet) (lo, hi []float64) {
	n := len(instruments)
	lo = make([]float64, n)
	hi = make([]float64, n)
	for i, id := range instruments {
		l, h := defaultLo(cons), defaultHi(cons)
		if cons != nil {
			if b, ok := cons.Bounds[id]; ok {
				l, h = b.Min, b.Max
			}
		}
		if h < l {
			h = l
		}
		lo[i], hi[i] = l, h
	}
	return lo, hi
}

func defaultLo(cons *ConstraintSet) float64 {
	if cons == nil {
		return 0 // long-only default
	}
	if cons.LongOnly {
		return math.Max(0, cons.MinWeight)
	}
	if cons.MinWeight == 0 {
		return -1 // a sane short floor when none specified
	}
	return cons.MinWeight
}

func defaultHi(cons *ConstraintSet) float64 {
	if cons == nil || cons.MaxWeight == 0 {
		return 1
	}
	return cons.MaxWeight
}

// MandateConstraints derives a ConstraintSet from a COMP-01 mandate so the
// optimizer respects the rules it CAN express as constraints directly: an
// INSTRUMENT-dimension concentration limit becomes that instrument's max weight;
// a DENY restriction-list value becomes a 0 max weight (cannot hold it); a
// SECTOR concentration limit becomes a sector cap. Rules the box can't express
// (leverage, currency, issuer aggregation) are left to CheckMandate. classifier
// resolves sector buckets; nil ⇒ sector caps are skipped here (still checked
// post-hoc).
func MandateConstraints(mandate *compliancepb.Mandate, longOnly bool) *ConstraintSet {
	cs := &ConstraintSet{LongOnly: longOnly, Bounds: map[string]WeightBound{}, SectorCaps: map[string]float64{}}
	for _, rule := range mandate.GetRules() {
		switch rule.GetType() {
		case compliancepb.RuleType_RULE_TYPE_CONCENTRATION:
			cl := rule.GetConcentration()
			max := dec.Float64Or(cl.GetMaxWeight(), 0)
			switch cl.GetDimension() {
			case compliancepb.Dimension_DIMENSION_INSTRUMENT:
				if cl.GetBucket() != "" {
					cs.Bounds[cl.GetBucket()] = WeightBound{Min: loFloor(longOnly), Max: max}
				}
			case compliancepb.Dimension_DIMENSION_SECTOR:
				if cl.GetBucket() != "" {
					cs.SectorCaps[cl.GetBucket()] = max
				}
			}
		case compliancepb.RuleType_RULE_TYPE_RESTRICTION:
			rl := rule.GetRestriction()
			if rl.GetDimension() == compliancepb.Dimension_DIMENSION_INSTRUMENT &&
				rl.GetMode() == compliancepb.RestrictionMode_RESTRICTION_MODE_DENY {
				for _, id := range rl.GetValues() {
					cs.Bounds[id] = WeightBound{Min: 0, Max: 0} // cannot hold
				}
			}
		}
	}
	return cs
}

func loFloor(longOnly bool) float64 {
	if longOnly {
		return 0
	}
	return -1
}

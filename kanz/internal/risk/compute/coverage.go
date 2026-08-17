package compute

import (
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
)

// Coverage accumulates one measure's v1.InputCoverage while the measure runs.
//
// # One accumulator for every measure family (#527)
//
// The FI, factor, XVA and historical-VaR measures each had the same shape: walk
// the book, skip whatever does not resolve, return the accumulator whether or
// not anything reached it. Fixing that four times would have produced four
// spellings of "what did this measure fail to see", and the estate has already
// paid for that mistake once — #257 was ten copies of the base-currency filter,
// of which not one reported what it dropped. This is the single place the answer
// is assembled, so a fifth family gets the behaviour by using it rather than by
// reimplementing it, and the sample bound cannot differ between families.
//
// NOT SAFE FOR CONCURRENT USE, and it does not need to be: a MeasureFunc is
// called with one portfolio at a time and a coverage lives entirely inside one
// such call. The registry's closures are shared across concurrent queries;
// nothing here is.
type Coverage struct {
	// Contributed is incremented by the caller when a position reaches the
	// arithmetic. Left to the caller rather than inferred, because only the
	// measure knows what "contributed" means for it — a position with terms and a
	// curve still contributes nothing to a weighted average if its MarketValue is
	// zero.
	Contributed int

	excluded int
	sample   []v1.InputExclusion
}

// Exclude records one position the measure could not assess. Reasons come from
// the family's own closed vocabulary (SkipNoTerms, SkipNoModel, ...).
//
// THE COUNT IS UNBOUNDED AND THE SAMPLE IS NOT. On a book whose reference data
// was never loaded every position is an exclusion, so keeping them all would put
// the entire book in every response and in the degraded cache. The count is what
// says how bad it is; the sample is what lets somebody go and look.
func (c *Coverage) Exclude(id domain.InstrumentID, reason string) {
	c.excluded++
	if len(c.sample) < v1.MaxInputExclusions {
		c.sample = append(c.sample, v1.InputExclusion{
			InstrumentID: v1.InstrumentID(id),
			Reason:       reason,
		})
	}
}

// ExcludeWhole records an exclusion that belongs to the evaluation rather than
// to any holding — no factor model for this snapshot, no counterparty exposures,
// too little history to form a distribution. It counts as ONE exclusion with an
// empty instrument id, which is v1.InputExclusion's documented whole-book form.
//
// It is deliberately not "one exclusion per position": the measure did not
// assess the book position by position and then fail on each, it never got as
// far as the book at all, and inflating the count to the position count would
// make a whole-book outage look like a per-instrument data gap.
func (c *Coverage) ExcludeWhole(reason string) {
	c.Exclude("", reason)
}

// Result freezes the accumulator into the api/v1 shape carried on the measure.
func (c *Coverage) Result() v1.InputCoverage {
	return v1.InputCoverage{
		Contributed:   c.Contributed,
		ExcludedCount: c.excluded,
		Exclusions:    c.sample,
	}
}

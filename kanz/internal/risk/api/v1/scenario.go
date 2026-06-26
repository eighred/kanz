package v1

import (
	"fmt"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
)

// Concrete ScenarioShock types — the baseline taxonomy external
// callers construct and pass through ScenarioRequest.Shocks. New
// shock types are added here (under additive-only v1 discipline)
// and dispatched by the internal scenario package (RISK-09).
//
// # Why these types live in api/v1, not in scenario
//
// RISK-02's arch test forbids outsiders from importing
// kanz/internal/risk/scenario. But external callers need to
// CONSTRUCT shocks to drive Engine.EvaluateScenario. The api/v1
// package is the only risk subtree outsiders may import, so the
// shock value types live here. The scenario engine (RISK-09) owns
// the dispatch + apply — both halves type-assert on these api/v1
// types.
//
// # Adding a shock type
//
// 1. Add the struct + Description() method here.
// 2. Add the apply case in kanz/internal/risk/scenario.
// 3. The Engine.EvaluateScenario path picks it up automatically.
//
// Per the additive-only v1 rule, removing or renaming a shock type
// is a breaking change requiring a v2/ package.

// PriceShock applies a percentage change to a specific instrument's
// MarketValue. Pct is the fractional change (e.g. -0.10 means
// "drop the price by 10%"); use a Decimal with coefficient -10 and
// exponent -2 for that example.
//
// A shock against an instrument the portfolio does not hold is a
// silent no-op — the scenario engine processes it without error so
// large scenario batches (1000 shocks across a watchlist) succeed
// regardless of which instruments any given portfolio actually
// holds.
type PriceShock struct {
	InstrumentID InstrumentID
	Pct          *commonpb.Decimal
}

// Description satisfies ScenarioShock.
func (s PriceShock) Description() string {
	return fmt.Sprintf("price shock %s by %s%%", s.InstrumentID, formatDecimal(s.Pct))
}

// ParallelShift applies the same percentage shock to every position
// with a non-nil MarketValue. The textbook stress test ("what if
// everything drops 5%?") plus the simplest sanity check for a new
// risk model.
type ParallelShift struct {
	Pct *commonpb.Decimal
}

// Description satisfies ScenarioShock.
func (s ParallelShift) Description() string {
	return fmt.Sprintf("parallel shift %s%%", formatDecimal(s.Pct))
}

// SectorShock applies a percentage change to every position whose
// instrument classifies into the given sector — taxonomy + code, e.g.
// {"GICS","40"} for financials (mirrors reference.v1.SectorClassification
// and factor.Sector, but carried as plain strings because api/v1 is the
// leaf package external callers import and cannot depend on the internal
// factor package). Pct is the fractional change, same convention as
// PriceShock.
//
// Sector membership is resolved at apply time against the factor model
// the scenario engine is wired with (MODEL-01f). When no classifier is
// wired the shock is a silent no-op — same degradation as an unknown
// shock type. This is the building block for differentiated stress
// scenarios (MODEL-01h): a 2008 replay shocks financials harder than
// staples, which a ParallelShift cannot express.
type SectorShock struct {
	Taxonomy string
	Code     string
	Pct      *commonpb.Decimal
}

// Description satisfies ScenarioShock.
func (s SectorShock) Description() string {
	return fmt.Sprintf("sector shock %s:%s by %s%%", s.Taxonomy, s.Code, formatDecimal(s.Pct))
}

// VolShock bumps the implied volatility used to reprice option positions, by an
// absolute amount (AbsBump = 0.05 ⇒ +5 vol points). It only has an effect under
// a full-revaluation scenario (EvaluateReval, DERIV-01e), where option positions
// are repriced through Black-Scholes; the linear MarketValue path cannot express
// a vega effect and treats it as a no-op. An empty UnderlyingID applies the bump
// to every underlying — the textbook "vol up 5 points across the book" stress.
type VolShock struct {
	UnderlyingID InstrumentID
	AbsBump      *commonpb.Decimal
}

// Description satisfies ScenarioShock.
func (s VolShock) Description() string {
	target := "all"
	if s.UnderlyingID != "" {
		target = string(s.UnderlyingID)
	}
	return fmt.Sprintf("vol shock %s by %s (abs)", target, formatDecimal(s.AbsBump))
}

// formatDecimal is a tiny helper for Description strings — not
// general-purpose Decimal formatting (that would belong in
// common.v1 or a future text-format helper). Just enough to make
// audit logs readable.
func formatDecimal(d *commonpb.Decimal) string {
	if d == nil {
		return "0"
	}
	return fmt.Sprintf("%d×10^%d", d.Coefficient, d.Exponent)
}

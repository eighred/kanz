// Package measureread projects a governed risk read into what an agent-facing
// plane may state (#757).
//
// # A RECORD THE CALLER NEVER RECEIVES IS NOT A RECORD, ONE HOP FURTHER OUT
//
// #509 and #527 gave every measure an InputCoverage — how many positions reached
// the arithmetic, how many could not be assessed, and a bounded sample of which —
// and got it onto the wire beside the response's as_of and quality flags. The
// engine populates all of it. Both agent-facing consumers then flattened the
// whole thing to map[string]float64 with dec.Float64Or(value, 0), which is this
// repository's own greppable marker for a confident zero (internal/dec/float.go).
//
// The consumer on this hop is a language model, and it states what it is handed
// as prose. The fixed-income family is registered in production over a
// contract-terms store with no production writer, so DV01 is a zero over zero
// bonds TODAY — and an agent handed {"DV01": 0} answers "your book carries no
// interest-rate risk", byte-identical to the truth for a book holding no bonds.
// That is the exact conflation InputCoverage was made a message to break.
//
// # WHY WITHHOLDING, AND NOT ANNOTATING
//
// The alternative was to keep the number and attach the coverage to it. It was
// rejected for two reasons. domain.v1.InputCoverage's own doc is unambiguous — "a caller gating
// on a money measure must treat a non-zero excluded_count as a REFUSAL TO
// ANSWER, not as an annotation on a good number" — because dropping positions
// shrinks a sum but RAISES a ratio or a portfolio quantile, so a partial number
// is not a smaller number, it is an unknown one. And a model asked to summarise
// "VaR99 = 1.2m (computed over 499 of 500 positions)" reliably drops the
// parenthesis. Withholding costs an operator nothing they had: the reason
// carries the contributed/excluded counts and the exclusion reasons, so they
// learn WHICH store is empty instead of reading a number that measured nothing.
//
// # ONE IMPLEMENTATION, TWO CONSUMERS
//
// services/mcp and services/copilot both read query.v1 and both hand the result
// to an agent. "May this number be stated" is one decision, and a second copy is
// how a fix stops spreading. It lives here for the same reason internal/agentgate
// does: a second consumer arrived, so the seam was promoted rather than copied.
// The projections themselves stay separate — each plane decides what SHAPE it
// exposes; this decides what may be exposed at all.
package measureread

import (
	"fmt"
	"sort"
	"strings"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"

	"github.com/eighred/kanz/internal/dec"
)

// Status is whether a value may be stated. It is deliberately two-valued and
// not three: "the engine did not say" and "the engine said something that makes
// this unsafe to state" are one answer to an agent, which must not act on the
// number either way. What separates them is Reason, which is prose for an
// operator rather than a branch for a caller.
type Status string

const (
	// StatusMeasured: the engine produced a value and asserted nothing that
	// makes it unsafe to state. Value is set.
	StatusMeasured Status = "MEASURED"
	// StatusUnavailable: this plane will not state a number here. Value is nil —
	// not zero, not the engine's zero, nothing. Reason says why.
	StatusUnavailable Status = "UNAVAILABLE"
)

// Coverage is how much of the book one value was computed over.
//
// REPORTED IS A FIELD BECAUSE PRESENCE IS THE SIGNAL. domain.v1.InputCoverage is
// a message and not two scalars precisely so that "does not report coverage" and
// "reports coverage, and everything resolved" cannot render identically
// (see domain.v1.InputCoverage). Flattening it here would re-create the conflation on the
// last hop.
type Coverage struct {
	// Reported is false when the measure carried no coverage record at all.
	// That is NOT a claim that everything resolved.
	Reported bool `json:"reported"`
	// Contributed is how many positions reached the arithmetic. Zero with a
	// non-zero Excluded means computed over nothing at all.
	Contributed uint32 `json:"contributed,omitempty"`
	// Excluded is the TOTAL count of positions that could not be assessed, not
	// the length of ExclusionReasons.
	Excluded uint32 `json:"excluded,omitempty"`
	// ExclusionReasons is the deduplicated, ordered set of reasons from the
	// bounded sample the engine attached — "no_terms", "no_curve". It is what
	// tells an operator which store is empty, and it is low-cardinality by the
	// schema's own contract.
	ExclusionReasons []string `json:"exclusion_reasons,omitempty"`
}

// Measure is one governed value and whether it may be stated.
type Measure struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	// Value is a pointer so that a withheld measure marshals with NO number at
	// all. A float64 field would render 0 and put back exactly the confident
	// zero this package exists to remove.
	Value *float64 `json:"value,omitempty"`
	// UncertaintyAbs is the one-sigma band when the engine propagated one.
	// Absent means none was propagated — distinct from a zero band, which would
	// claim a calibrated point estimate (see domain.v1.RiskMeasure.uncertainty_abs).
	UncertaintyAbs *float64 `json:"uncertainty_abs,omitempty"`
	// Reason is why the value is withheld. Empty when Status is MEASURED.
	Reason   string   `json:"reason,omitempty"`
	Coverage Coverage `json:"coverage"`
}

// Set is a projected governed read.
type Set struct {
	PortfolioID string `json:"portfolio_id"`
	// AsOf is the domain time the state is effective at. Nil means the engine
	// stamped none — said rather than rendered as the zero instant, which would
	// be a lie about how current the state is.
	AsOf *time.Time `json:"as_of"`
	// QualityFlags names every trust signal the engine attached, and is ALWAYS
	// non-nil. An absent key would leave a reader inferring trust from silence,
	// which is the two-state trap #675 removed.
	QualityFlags []string `json:"quality_flags"`
	// Measures is ordered by name. Two identical reads must produce the same
	// agent context, and a map would not.
	Measures []Measure `json:"measures"`
}

// ProjectMeasures projects a query.v1 measures response.
func ProjectMeasures(resp *querypb.MeasuresResponse) Set {
	flags, withholdAll, flagReason := readFlags(resp.GetQualityFlags())
	set := Set{
		PortfolioID:  resp.GetPortfolioId(),
		AsOf:         stamp(resp.GetAsOf().AsTime(), resp.GetAsOf() != nil),
		QualityFlags: flags,
	}
	for _, m := range resp.GetSet().GetMeasures() {
		set.Measures = append(set.Measures, projectMeasure(m, withholdAll, flagReason))
	}
	sortByName(set.Measures)
	return set
}

// ProjectScenario projects a query.v1 scenario evaluation.
//
// A what-if is a pure function of state the caller was already authorized for,
// so it carries no owning tenant and no as_of — but it carries the SAME quality
// flags and the same per-measure coverage, because it was computed from the same
// inputs. A scenario projected over a book whose terms never loaded is exactly
// as empty as the measure it was shocked from, and moving no capital is not the
// same as stating nothing.
func ProjectScenario(resp *querypb.EvaluateScenarioResponse) Set {
	flags, withholdAll, flagReason := readFlags(resp.GetQualityFlags())
	set := Set{PortfolioID: resp.GetPortfolioId(), QualityFlags: flags}
	for _, m := range resp.GetProjected().GetMeasures() {
		set.Measures = append(set.Measures, projectMeasure(m, withholdAll, flagReason))
	}
	sortByName(set.Measures)
	return set
}

// ProjectExposure projects a query.v1 exposure response.
//
// ExposureState carries no InputCoverage — the decomposition is a sum over
// positions the engine already holds, not a resolve through a provider — so the
// per-value question here is convertibility alone. The response-level flags
// apply exactly as they do to measures, and CURRENCY_EXCLUDED matters MORE: it
// is a currency filter, and one of these dimensions is currency.
func ProjectExposure(resp *querypb.ExposureResponse) Set {
	flags, withholdAll, flagReason := readFlags(resp.GetQualityFlags())
	set := Set{
		PortfolioID:  resp.GetPortfolioId(),
		AsOf:         stamp(resp.GetAsOf().AsTime(), resp.GetAsOf() != nil),
		QualityFlags: flags,
	}
	for _, e := range resp.GetSet().GetExposures() {
		// The composite key avoids collisions when the same bucket name appears
		// under two dimensions.
		m := Measure{Name: e.GetDimension().String() + "/" + e.GetBucket()}
		set.Measures = append(set.Measures, stateValue(m, e.GetNet().GetAmount(), nil, withholdAll, flagReason))
	}
	sortByName(set.Measures)
	return set
}

func projectMeasure(m *domainpb.RiskMeasure, withholdAll bool, flagReason string) Measure {
	out := Measure{Name: m.GetName(), Coverage: coverageOf(m.GetCoverage())}
	out = stateValue(out, m.GetValue(), m.GetCoverage(), withholdAll, flagReason)
	if out.Status == StatusMeasured {
		if u, ok := dec.Float64(m.GetUncertaintyAbs()); ok {
			out.UncertaintyAbs = &u
		}
	}
	return out
}

// stateValue is the whole decision, and the ORDER OF THE CHECKS IS THE
// SEMANTICS: a response-wide refusal outranks a per-value one, and coverage
// outranks convertibility because a value that converts perfectly is the
// dangerous case when it was computed over nothing.
func stateValue(out Measure, value *commonpb.Decimal, cov *domainpb.InputCoverage, withholdAll bool, flagReason string) Measure {
	if withholdAll {
		return withhold(out, flagReason)
	}
	if cov != nil && cov.GetExcludedCount() > 0 {
		return withhold(out, fmt.Sprintf(
			"computed over %d position(s) with %d excluded (%s) — dropping positions moves a ratio "+
				"or a quantile in an unknown direction, so this is a refusal to answer rather than a "+
				"smaller number",
			cov.GetContributed(), cov.GetExcludedCount(), reasonList(cov)))
	}
	v, ok := dec.Float64(value)
	if !ok {
		return withhold(out, "the engine returned no readable value for this measure")
	}
	out.Status = StatusMeasured
	out.Value = &v
	return out
}

func withhold(out Measure, reason string) Measure {
	out.Status = StatusUnavailable
	out.Value = nil
	out.Reason = reason
	return out
}

// readFlags names every flag and decides whether the response withholds wholesale.
//
// TWO FLAGS WITHHOLD AND TWO DO NOT, and which is which is the argument:
//
//   - CURRENCY_EXCLUDED withholds. The response was computed over a SUBSET of the
//     book because the engine has no FX layer (RISK-06), and QUALITY_FLAG_CURRENCY_EXCLUDED
//     says a caller that must not under-report has to refuse rather than treat it
//     as a number. No direction claim is available to make instead.
//   - UNSPECIFIED withholds. grpcsrv emits it for a flag it could not map and says
//     a caller must not read it as "fine". An unnamed trust signal is an unknown,
//     and a critical unknown fails closed.
//   - DEGRADED and STALE do NOT withhold. The values are real, just cached or
//     older than the request's budget; the named flag and as_of say exactly that.
//     Emptying the plane here would empty it during precisely the incident an
//     operator is investigating.
//   - INPUTS_UNRESOLVED does NOT withhold on its own. It is the set-level echo of
//     per-measure coverage, and the per-measure rule above withholds exactly the
//     measures it names. #509 was filed because the set-level flag alone forced a
//     caller to refuse everything or nothing; refusing wholesale here would spend
//     what that issue bought.
func readFlags(flags []querypb.QualityFlag) (names []string, withholdAll bool, reason string) {
	names = []string{}
	for _, f := range flags {
		names = append(names, strings.TrimPrefix(f.String(), "QUALITY_FLAG_"))
		switch f {
		case querypb.QualityFlag_QUALITY_FLAG_CURRENCY_EXCLUDED:
			withholdAll = true
			reason = "the response was computed over a currency subset of the book " +
				"(CURRENCY_EXCLUDED); every base-currency value on it may move in either direction"
		case querypb.QualityFlag_QUALITY_FLAG_UNSPECIFIED:
			if !withholdAll {
				withholdAll = true
				reason = "the engine attached a trust flag this platform cannot name " +
					"(QUALITY_FLAG_UNSPECIFIED); an unnamed signal is an unknown, not an all-clear"
			}
		}
	}
	return names, withholdAll, reason
}

func coverageOf(c *domainpb.InputCoverage) Coverage {
	if c == nil {
		return Coverage{Reported: false}
	}
	return Coverage{
		Reported:         true,
		Contributed:      c.GetContributed(),
		Excluded:         c.GetExcludedCount(),
		ExclusionReasons: reasons(c),
	}
}

// reasons deduplicates the bounded exclusion sample down to its reason
// vocabulary. The instrument ids are deliberately NOT carried: they are a
// bounded sample rather than the whole set, so an agent shown three of two
// hundred would reasonably read them as the complete list.
func reasons(c *domainpb.InputCoverage) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range c.GetExclusions() {
		r := e.GetReason()
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func reasonList(c *domainpb.InputCoverage) string {
	r := reasons(c)
	if len(r) == 0 {
		// The engine reported a count without a sample. Saying so beats an empty
		// parenthesis that reads as "excluded for no reason".
		return "no reason sampled"
	}
	return strings.Join(r, ", ")
}

func stamp(t time.Time, present bool) *time.Time {
	if !present || t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func sortByName(ms []Measure) {
	sort.Slice(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
}

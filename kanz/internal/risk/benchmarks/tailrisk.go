package benchmarks

import (
	"context"
	"math"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	riskv1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	varmodel "github.com/eighred/kanz/internal/risk/compute/var"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/validation"
)

// HISTORICAL VALUE-AT-RISK AND EXPECTED SHORTFALL (#471).
//
// # What this validates, precisely
//
// varmodel.Historical and varmodel.ExpectedShortfall AS DEPLOYED — through a
// real domain.Portfolio and a real compute.ReturnsProvider, not by reaching into
// the estimator. Every case below therefore also crosses portfolioPnL, the
// base-currency filter and the tail-alignment, because a VaR that is right about
// the wrong P&L series is wrong in exactly the way nobody notices.
//
// The two are listed separately in Inventory() and validated separately here,
// for the reason stated there: they share a distribution but not an estimator,
// and one benchmark must not mark two analytics green.
//
// # This set found a defect, which is the argument for the whole issue
//
// ES computed its tail count as int(math.Ceil(n·(1−α))). 1−0.99 is
// 0.01000000000000000888 in float64, so n·(1−α) lands a hair ABOVE an integer
// whenever it should land exactly on one, and Ceil returned one too many: at
// n=100, α=0.99 it averaged the 2 worst scenarios instead of the 1. An extra
// scenario in a tail mean is a LESS BAD one, so ES came out SMALLER — the tail
// measure understated the tail, in the direction nobody questions.
//
// It had survived because the estimator's own documented example (n=250,
// α=0.99 ⇒ k=3) is one of the cases the wrong expression gets right, and so is
// α=0.90 at n=100. The ramp cases below are at 0.99, 0.95 AND 0.90 for that
// reason: the third is the control that shows the defect is confidence-
// dependent, and a set that had checked one confidence would have passed.
//
// # Three kinds of evidence
//
// EXACT ARITHMETIC on a hand-computable distribution. A P&L ramp of −1…−100 has
// a mean-of-k-worst that can be done in the head, so the k-count cases carry a
// 1e-9 tolerance and pin the estimator's discrete behaviour with no room to
// argue. These are the cases that catch the defect above.
//
// THE NORMAL CLOSED FORM, cross-model. On a normal distribution VaR_α = σz_α
// and ES_α = σφ(z_α)/(1−α), both published. The empirical estimator on a
// stratified sample approaches these from below — the deviation is the
// stratification's own discretisation, and it is asserted as CONVERGENCE rather
// than a point tolerance, because a fixed tolerance here would be a number
// fitted to one sample size. Measured: at α=0.99 the VaR deviation runs 0.352,
// 0.180, 0.091, 0.046 at n=500, 1000, 2000, 4000 — halving with n, which is
// what discretisation looks like and what an error does not.
//
// COHERENCE, on the textbook counterexample. Two independent positions that
// each default with probability 4%: at 95%, neither default alone reaches the
// quantile, but P(either defaults) = 1 − 0.96² = 7.84% does. So VaR is
// SUPERADDITIVE here — VaR(A+B) = 500 against VaR(A) + VaR(B) = 0 — and ES is
// not. That is the published pathology behind Basel's move from VaR to ES in
// FRTB, and reproducing it on the same fixture is the sharpest available
// evidence that these are the measures they claim to be rather than two
// spellings of one quantile. It is asserted in BOTH directions on purpose: a VaR
// that came out subadditive here would be a VaR that is secretly an ES.

// AnalyticValueAtRisk and AnalyticExpectedShortfall are the two tail measures.
const (
	AnalyticValueAtRisk       = "value_at_risk"
	AnalyticExpectedShortfall = "expected_shortfall"
)

// tailAsOf is the portfolio state time. Fixed, because the provider below
// ignores it and a benchmark must not vary with the wall clock.
var tailAsOf = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

// legReturns is a per-instrument return series provider — per-instrument rather
// than fixed, because the coherence cases need two legs with DIFFERENT series
// and a provider that answers the same for both would make every portfolio
// perfectly correlated and subadditivity vacuous.
type legReturns map[string][]float64

func (l legReturns) Returns(_ context.Context, id string, _ time.Time, _ int) ([]float64, error) {
	return l[id], nil
}

// tailPortfolio builds a USD portfolio of unit-priced legs. Each leg's value is
// its weight, so the per-scenario P&L is Σ weight_i × return_i — which is what
// lets every case below state its expectation in money directly.
func tailPortfolio(values map[string]int64) *domain.Portfolio {
	p := domain.NewPortfolio(riskv1.PortfolioID("BENCH"), domain.CurrencyCode("USD"))
	p.SetAggregate(domain.AggregateUpdate{AsOf: tailAsOf, BaseCurrency: domain.CurrencyCode("USD")})
	for id, v := range values {
		p.SetPosition(domain.Position{
			InstrumentID: riskv1.InstrumentID(id),
			MarketValue: &commonpb.Money{
				Amount:       &commonpb.Decimal{Coefficient: v, Exponent: 0},
				CurrencyCode: "USD",
			},
		})
	}
	return p
}

// tailMeasure runs one measure over one fixture and returns the emitted value.
func tailMeasure(m compute.ReturnsMeasure, p *domain.Portfolio, r legReturns) float64 {
	return decimalFloat(m(context.Background(), p, r).Value)
}

func decimalFloat(d *commonpb.Decimal) float64 {
	if d == nil {
		return 0
	}
	return float64(d.Coefficient) * math.Pow10(int(d.Exponent))
}

// varAt and esAt evaluate the two measures at a confidence over a single-leg
// fixture whose P&L series IS the supplied series (the leg is worth 1).
func varAt(pnl []float64, conf float64) float64 {
	return tailMeasure(varmodel.Historical(varmodel.Config{Confidence: conf}),
		tailPortfolio(map[string]int64{"X": 1}), legReturns{"X": pnl})
}

func esAt(pnl []float64, conf float64) float64 {
	return tailMeasure(varmodel.ExpectedShortfall(varmodel.Config{Confidence: conf}),
		tailPortfolio(map[string]int64{"X": 1}), legReturns{"X": pnl})
}

// ramp is the P&L series −1, −2, … −n. Every mean-of-k-worst over it is an
// integer average that can be checked by hand, which is what makes the k-count
// cases exact rather than approximate.
func ramp(n int) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = -float64(i + 1)
	}
	return s
}

// stratifiedNormal is a deterministic sample of N(0,σ) at the midpoints of n
// equal-probability strata: x_i = σ·Φ⁻¹((i+½)/n).
//
// DETERMINISTIC ON PURPOSE. A pseudo-random sample would make the deviation from
// the closed form a draw of the RNG, so the tolerance would have to cover
// sampling noise — and a benchmark whose tolerance covers sampling noise cannot
// see an error smaller than the noise. Stratifying removes the randomness
// entirely and leaves a discretisation that shrinks predictably with n, which is
// the property the convergence cases below assert.
func stratifiedNormal(n int, sigma float64) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = sigma * normInv((float64(i)+0.5)/float64(n))
	}
	return s
}

// normInv is Φ⁻¹ via the error function: Φ⁻¹(p) = √2·erfinv(2p−1).
func normInv(p float64) float64 { return math.Sqrt2 * math.Erfinv(2*p-1) }

// normPDFStd is the standard normal density, for the ES closed form.
func normPDFStd(x float64) float64 { return math.Exp(-0.5*x*x) / math.Sqrt(2*math.Pi) }

// ValueAtRisk grades historical-simulation VaR.
func ValueAtRisk() []validation.Case {
	r100 := ramp(100)

	// The five-scenario distribution the package's own unit test uses, so the
	// benchmark and the test agree on the same arithmetic rather than each
	// asserting its own: P&L {−100,−50,0,50,100}, and the 1% lower-tail quantile
	// interpolates 4% of the way from −100 to −50.
	five := []float64{-0.10, -0.05, 0, 0.05, 0.10}

	cases := []validation.Case{
		{
			// EXACT. h = (n−1)·(1−α) = 4×0.01 = 0.04 ⇒ −100 + 0.04×50 = −98.
			Name: "historical_var_is_the_interpolated_empirical_quantile_definitional",
			Got:  varAt(scale(five, 1000), 0.99), Want: 98, Tolerance: 1e-9,
		},
		{
			// EXACT, on the ramp: h = 99×0.01 = 0.99 ⇒ −100 + 0.99×1 = −99.01.
			Name: "historical_var_interpolates_between_adjacent_scenarios_definitional",
			Got:  varAt(r100, 0.99), Want: 99.01, Tolerance: 1e-9,
		},
		{
			// MONOTONE IN CONFIDENCE. A higher confidence cannot ask for a smaller
			// loss. Published, and it catches an inverted tail — a VaR computed
			// from the UPPER quantile still moves with α, just the wrong way.
			Name: "var_is_monotone_in_confidence_published",
			Got: boolAsFloat(varAt(r100, 0.99) >= varAt(r100, 0.95) &&
				varAt(r100, 0.95) >= varAt(r100, 0.90)), Want: 1, Tolerance: 0.5,
		},
		{
			// POSITIVE HOMOGENEITY: VaR(λX) = λ·VaR(X). A coherence axiom VaR does
			// satisfy, and the one that fails on any absolute constant smuggled
			// into the estimator — a floor, a cap, or a units error.
			Name: "var_is_positively_homogeneous_published",
			Got:  varAt(scale(r100, 3), 0.99), Want: 3 * varAt(r100, 0.99), Tolerance: 1e-9,
		},
		{
			// NON-NEGATIVE. A profitable tail is not a negative capital charge.
			Name: "var_floors_at_zero_on_a_non_loss_tail_definitional",
			Got:  varAt([]float64{0.01, 0.02, 0.03, 0.04, 0.05}, 0.99), Want: 0, Tolerance: 1e-12,
		},
	}

	// ===== CROSS-MODEL: the normal closed form =====
	const sigma = 0.01
	n2000 := stratifiedNormal(2000, sigma)
	cases = append(cases,
		validation.Case{
			// VaR_α = σ·z_α. Tolerance from the measured envelope (0.091 at
			// n=2000), not from precision: the estimator approaches this from
			// below and is SUPPOSED to differ at finite n.
			Name: "var_approaches_sigma_times_z_alpha_on_a_normal_cross_model",
			Got:  varAt(scale(n2000, 1000), 0.99), Want: normalVaR(sigma, 0.99), Tolerance: 0.15,
		},
		validation.Case{
			// AND THE GAP IS DISCRETISATION, NOT ERROR — it shrinks with n. This
			// is the sharper of the two: a systematic bias would not converge, and
			// no point tolerance can tell the difference.
			Name: "var_deviation_from_the_closed_form_shrinks_with_sample_size_published",
			Got: boolAsFloat(normalGap(2000, sigma, 0.99, varAt, normalVaR) <
				normalGap(1000, sigma, 0.99, varAt, normalVaR)), Want: 1, Tolerance: 0.5,
		},
	)

	return append(cases, coherenceCases(false)...)
}

// ExpectedShortfall grades the ES99/CVaR tail mean.
func ExpectedShortfall() []validation.Case {
	r100 := ramp(100)

	cases := []validation.Case{
		// ===== THE k-COUNT CASES: exact, and the ones that found the defect =====
		{
			// n=100, α=0.99 ⇒ k = 1. ES is the single worst scenario, −(−100).
			// THE SHARPEST OF THE THREE: one extra scenario is a 100% error in the
			// count, and the wrong expression returned 99.50 here.
			Name: "es_averages_exactly_one_scenario_at_99_over_100_definitional",
			Got:  esAt(r100, 0.99), Want: 100, Tolerance: 1e-9,
		},
		{
			// n=100, α=0.95 ⇒ k = 5: mean(−100…−96) = −98. Wrong expression: 97.50.
			Name: "es_averages_exactly_five_scenarios_at_95_over_100_definitional",
			Got:  esAt(r100, 0.95), Want: 98, Tolerance: 1e-9,
		},
		{
			// n=100, α=0.90 ⇒ k = 10: mean(−100…−91) = −95.5. THE CONTROL. The
			// wrong expression got this one RIGHT, which is why it survived — a
			// set that had checked a single confidence would have passed while ES
			// understated the tail at the two confidences anyone reports.
			Name: "es_averages_exactly_ten_scenarios_at_90_over_100_definitional",
			Got:  esAt(r100, 0.90), Want: 95.5, Tolerance: 1e-9,
		},
		{
			// SMALL-WINDOW FLOOR: k is floored at 1, so 5 scenarios at 99% still
			// yield the single worst loss rather than an empty average.
			Name: "es_floors_at_the_single_worst_scenario_on_a_short_window_definitional",
			Got:  esAt(scale([]float64{-0.10, -0.05, 0, 0.05, 0.10}, 1000), 0.99),
			Want: 100, Tolerance: 1e-9,
		},

		// ===== THE DEFINING RELATION =====
		{
			// ES ≥ VaR AT EVERY CONFIDENCE. The mean of the tail cannot be milder
			// than its threshold. The estimator's own doc claims this holds "by
			// construction"; construction is not evidence.
			Name: "es_is_at_least_var_at_every_confidence_published",
			Got: boolAsFloat(esAt(r100, 0.99) >= varAt(r100, 0.99) &&
				esAt(r100, 0.95) >= varAt(r100, 0.95) &&
				esAt(r100, 0.90) >= varAt(r100, 0.90)), Want: 1, Tolerance: 0.5,
		},
		{
			Name: "es_is_monotone_in_confidence_published",
			Got: boolAsFloat(esAt(r100, 0.99) >= esAt(r100, 0.95) &&
				esAt(r100, 0.95) >= esAt(r100, 0.90)), Want: 1, Tolerance: 0.5,
		},
		{
			Name: "es_is_positively_homogeneous_published",
			Got:  esAt(scale(r100, 3), 0.95), Want: 3 * esAt(r100, 0.95), Tolerance: 1e-9,
		},
	}

	// ===== CROSS-MODEL: the normal closed form =====
	const sigma = 0.01
	n2000 := stratifiedNormal(2000, sigma)
	cases = append(cases,
		validation.Case{
			// ES_α = σ·φ(z_α)/(1−α), published. Measured deviation 0.042 at
			// n=2000; the tolerance sits above the envelope across the range.
			Name: "es_approaches_sigma_phi_z_over_one_minus_alpha_on_a_normal_cross_model",
			Got:  esAt(scale(n2000, 1000), 0.99), Want: normalES(sigma, 0.99), Tolerance: 0.08,
		},
		validation.Case{
			Name: "es_deviation_from_the_closed_form_shrinks_with_sample_size_published",
			Got: boolAsFloat(normalGap(2000, sigma, 0.99, esAt, normalES) <
				normalGap(1000, sigma, 0.99, esAt, normalES)), Want: 1, Tolerance: 0.5,
		},
	)

	return append(cases, coherenceCases(true)...)
}

// normalGap is |estimate − closed form| on a stratified normal of size n. Both
// the measure and the form it converges to are passed in, so VaR and ES share
// one convergence harness and cannot drift apart in how they are graded.
func normalGap(n int, sigma, conf float64,
	measure func([]float64, float64) float64, closedForm func(sigma, conf float64) float64,
) float64 {
	got := measure(scale(stratifiedNormal(n, sigma), 1000), conf)
	return math.Abs(got - closedForm(sigma, conf))
}

// The two published closed forms on a normal distribution, in P&L money for the
// 1000-unit leg the fixtures use.
func normalVaR(sigma, conf float64) float64 {
	return 1000 * sigma * normInv(conf)
}

func normalES(sigma, conf float64) float64 {
	return 1000 * sigma * normPDFStd(normInv(conf)) / (1 - conf)
}

// scale multiplies a series, so one fixture can be reused at several sizes.
func scale(s []float64, by float64) []float64 {
	out := make([]float64, len(s))
	for i, v := range s {
		out[i] = v * by
	}
	return out
}

// ===== COHERENCE: the published VaR counterexample =====
//
// Two independent legs, each losing 500 in 4% of scenarios. The scenario grid is
// 50×50 = 2500, so the marginals are EXACTLY 4% and the joint is exactly
// independent — a sampled version would make the whole demonstration a question
// of how the draw came out.
//
//	VaR95(A) = VaR95(B) = 0     — a 4% tail does not reach the 5% quantile
//	VaR95(A+B) = 500           — P(either) = 7.84% does
//	ES95(A) = ES95(B) = 400,  ES95(A+B) = 516 ≤ 800
//
// wantSub selects which measure is being graded: ES must be SUBADDITIVE, and VaR
// must be SUPERADDITIVE on this fixture. Asserting VaR's failure rather than
// skipping it is deliberate — a VaR that passed a subadditivity check here would
// be a VaR that is quietly computing something else.
func coherenceCases(wantSub bool) []validation.Case {
	const n, loss = 50, -0.5
	a := make([]float64, 0, n*n)
	b := make([]float64, 0, n*n)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			a = append(a, boolValue(i < 2, loss))
			b = append(b, boolValue(j < 2, loss))
		}
	}
	both := tailPortfolio(map[string]int64{"A": 1000, "B": 1000})
	one := tailPortfolio(map[string]int64{"A": 1000})
	series := legReturns{"A": a, "B": b}

	if wantSub {
		es := varmodel.ExpectedShortfall(varmodel.Config{Confidence: 0.95})
		joint := tailMeasure(es, both, series)
		single := tailMeasure(es, one, series)
		return []validation.Case{
			{
				Name: "es_of_the_pair_is_the_hand_computed_tail_mean_definitional",
				Got:  joint, Want: 516, Tolerance: 1e-9,
			},
			{
				// SUBADDITIVITY — the coherence property VaR lacks and ES has, and
				// the reason FRTB reports ES. Diversification cannot increase risk.
				Name: "es_is_subadditive_on_the_published_var_counterexample_published",
				Got:  boolAsFloat(joint <= 2*single), Want: 1, Tolerance: 0.5,
			},
		}
	}

	hist := varmodel.Historical(varmodel.Config{Confidence: 0.95})
	joint := tailMeasure(hist, both, series)
	single := tailMeasure(hist, one, series)
	return []validation.Case{
		{
			Name: "var_of_a_single_four_percent_default_leg_misses_the_tail_entirely_published",
			Got:  single, Want: 0, Tolerance: 1e-12,
		},
		{
			Name: "var_of_the_pair_is_the_full_single_default_loss_definitional",
			Got:  joint, Want: 500, Tolerance: 1e-9,
		},
		{
			// VaR IS SUPERADDITIVE HERE, and this asserts the failure. It is the
			// textbook counterexample (two independent defaultable positions), the
			// motivation for ES, and the single case that proves this VaR is a
			// quantile rather than a tail mean wearing a quantile's name.
			Name: "var_is_superadditive_on_the_published_counterexample_published",
			Got:  boolAsFloat(joint > 2*single), Want: 1, Tolerance: 0.5,
		},
	}
}

// boolValue returns v when cond, else 0 — a scenario grid built without a
// branch per cell.
func boolValue(cond bool, v float64) float64 {
	if cond {
		return v
	}
	return 0
}

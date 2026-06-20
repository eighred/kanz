package varmodel_test

import (
	"context"
	"math"
	"math/rand"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/kanz-eng/kanz/internal/marketdata/store"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	varmodel "github.com/kanz-eng/kanz/internal/risk/compute/var"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// MODEL-01i — the verification subtask that closes the MODEL-01 epic. Three
// properties a real VaR plane must hold: (1) it is statistically calibrated
// (backtest exception rate within a Kupiec band), (2) it reconciles to an
// independent closed-form reference, and (3) it is point-in-time-correct (a
// later data correction never leaks into an earlier backtest).

// z99 is the standard-normal 0.99 quantile — the delta-normal VaR multiplier
// used as the independent reference.
const z99 = 2.3263478740408408

// chi2_1_95 is the 95th percentile of χ²(1) — the Kupiec POF non-rejection
// threshold (the test does not reject calibration below it).
const chi2_1_95 = 3.841459

// sliceProvider hands back a settable return window — the backtest resets it per
// step to feed each rolling lookback through the real measure.
type sliceProvider struct{ ret []float64 }

func (s *sliceProvider) Returns(_ context.Context, _ string, _ time.Time, _ int) ([]float64, error) {
	return s.ret, nil
}

// normalReturns draws n iid N(0, sigma²) returns deterministically from seed.
func normalReturns(n int, sigma float64, seed int64) []float64 {
	rng := rand.New(rand.NewSource(seed))
	out := make([]float64, n)
	for i := range out {
		out[i] = rng.NormFloat64() * sigma
	}
	return out
}

func meanStd(xs []float64) (mean, std float64) {
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	for _, x := range xs {
		std += (x - mean) * (x - mean)
	}
	std = math.Sqrt(std / float64(len(xs)-1))
	return mean, std
}

// kupiecLR is the Kupiec proportion-of-failures likelihood-ratio statistic for
// x exceptions in n trials against expected failure probability p. Asymptotic
// χ²(1) under the null that the VaR is correctly calibrated.
func kupiecLR(n, x int, p float64) float64 {
	if x == 0 {
		return -2 * float64(n) * math.Log(1-p)
	}
	pi := float64(x) / float64(n)
	ll0 := float64(n-x)*math.Log(1-p) + float64(x)*math.Log(p)
	ll1 := float64(n-x)*math.Log(1-pi) + float64(x)*math.Log(pi)
	return -2 * (ll0 - ll1)
}

// TestHistorical_KupiecBacktest is the calibration check: roll a 250-day
// historical-VaR window across a long simulated return path, count one-day-ahead
// exceptions (realized loss exceeds the VaR predicted from the prior window),
// and assert the exception count passes the Kupiec POF test at 95%. A correctly
// calibrated 99% VaR exceeds ~1% of the time; deterministic via a fixed seed so
// the statistic is stable across runs.
func TestHistorical_KupiecBacktest(t *testing.T) {
	const (
		total  = 1300
		window = 250
		value  = 1_000_000.0
		sigma  = 0.012
		conf   = 0.99
	)
	hist := normalReturns(total, sigma, 20260601)
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(value, "USD")})
	prov := &sliceProvider{}
	model := varmodel.Historical(varmodel.Config{Confidence: conf, Window: window})

	exceptions, trials := 0, 0
	for tIdx := window; tIdx < total; tIdx++ {
		prov.ret = hist[tIdx-window : tIdx] // prior window, out-of-sample for day tIdx
		v := dval(model(context.Background(), p, prov).Value)
		if v <= 0 {
			t.Fatalf("step %d: VaR must be a positive loss, got %v", tIdx, v)
		}
		loss := -value * hist[tIdx] // realized one-day P&L loss
		if loss > v {
			exceptions++
		}
		trials++
	}

	lr := kupiecLR(trials, exceptions, 1-conf)
	rate := float64(exceptions) / float64(trials)
	t.Logf("backtest: %d exceptions / %d trials (%.3f%%), Kupiec LR=%.3f", exceptions, trials, rate*100, lr)
	if lr >= chi2_1_95 {
		t.Errorf("Kupiec POF rejects calibration: LR=%.3f >= %.3f (exceptions=%d/%d, rate=%.3f%%, expected ~1%%)",
			lr, chi2_1_95, exceptions, trials, rate*100)
	}
}

// TestVaR_ReconcilesToParametricReference reconciles both models against the
// independent delta-normal reference VaR = (z·σ − μ)·value computed from the
// same sample's moments. Monte-Carlo simulates exactly that normal, so it
// matches tightly; historical resampling matches within wider sampling error.
func TestVaR_ReconcilesToParametricReference(t *testing.T) {
	const (
		n     = 5000
		value = 1_000_000.0
		sigma = 0.02
	)
	rets := normalReturns(n, sigma, 424242)
	mean, std := meanStd(rets)
	ref := (z99*std - mean) * value // independent closed-form one-day 99% VaR

	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(value, "USD")})
	prov := &sliceProvider{ret: rets}

	mc := dval(varmodel.MonteCarlo(varmodel.Config{Draws: 200_000, Seed: 7})(context.Background(), p, prov).Value)
	if rel := math.Abs(mc-ref) / ref; rel > 0.025 {
		t.Errorf("Monte-Carlo VaR=%.0f vs reference=%.0f (%.2f%% off, want ≤2.5%%)", mc, ref, rel*100)
	}

	hist := dval(varmodel.Historical(varmodel.Config{})(context.Background(), p, prov).Value)
	if rel := math.Abs(hist-ref) / ref; rel > 0.08 {
		t.Errorf("historical VaR=%.0f vs reference=%.0f (%.2f%% off, want ≤8%%)", hist, ref, rel*100)
	}
}

// TestHistorical_PointInTimeNoFutureLeakage proves the bitemporal contract
// end-to-end through the VaR path: a late price correction (a past
// observation_time restated with a future knowledge_time) is invisible to a
// backtest as of an earlier knowledge horizon, and the early read matches a
// store that never had the correction. Only once the horizon advances past the
// correction's knowledge_time does it move the VaR.
func TestHistorical_PointInTimeNoFutureLeakage(t *testing.T) {
	const inst = "AAPL"
	day := func(n int) time.Time { return asOf.AddDate(0, 0, n) }
	dec := func(px float64) *commonpb.Decimal {
		return &commonpb.Decimal{Coefficient: int64(math.Round(px * 100)), Exponent: -2}
	}
	// Base close series, known same-day (knowledge_time == observation_time).
	closes := []float64{100, 101, 102, 103, 104, 105}
	base := make([]store.Observation, len(closes))
	for i, px := range closes {
		base[i] = store.Observation{
			InstrumentID: inst, ObservationTime: day(i), Price: dec(px),
			Kind: store.PriceKindClose, KnowledgeTime: day(i),
		}
	}
	// A late correction: restate day-2's close to a crash price, but only known
	// at day 20 — well after the day-5 backtest horizon.
	correction := store.Observation{
		InstrumentID: inst, ObservationTime: day(2), Price: dec(50),
		Kind: store.PriceKindClose, KnowledgeTime: day(20),
	}

	withCorr := store.NewMemory()
	if err := withCorr.Put(context.Background(), append(append([]store.Observation{}, base...), correction)); err != nil {
		t.Fatal(err)
	}
	withoutCorr := store.NewMemory()
	if err := withoutCorr.Put(context.Background(), base); err != nil {
		t.Fatal(err)
	}

	p := portfolio("USD", domain.Position{InstrumentID: inst, MarketValue: money(1_000_000, "USD")})
	model := varmodel.Historical(varmodel.Config{})
	varAsOf := func(s store.Store, at time.Time) float64 {
		prov := compute.NewStoreReturnsProvider(s, compute.ReturnsConfig{Method: compute.ReturnSimple})
		pAt := portfolioAsOf(p, at)
		return dval(model(context.Background(), pAt, prov).Value)
	}

	early := varAsOf(withCorr, day(5))    // horizon before the correction is known
	clean := varAsOf(withoutCorr, day(5)) // a store that never had the correction
	late := varAsOf(withCorr, day(30))    // horizon after the correction is known

	if math.Abs(early-clean) > 1e-6 {
		t.Errorf("future leakage: as-of-day-5 VaR=%.4f differs from the no-correction store=%.4f", early, clean)
	}
	if !(late > early) {
		t.Errorf("the correction must raise VaR once known: late=%.4f not > early=%.4f", late, early)
	}
}

// portfolioAsOf re-stamps p's positions at observation horizon `at` so the
// StoreReturnsProvider reads the bitemporal slice as of that knowledge time.
func portfolioAsOf(src *domain.Portfolio, at time.Time) *domain.Portfolio {
	p := domain.NewPortfolio(src.ID(), src.BaseCurrency())
	p.SetAggregate(domain.AggregateUpdate{AsOf: at, BaseCurrency: src.BaseCurrency()})
	for _, pos := range src.Positions() {
		pos.AsOf = at
		p.SetPosition(pos)
	}
	return p
}

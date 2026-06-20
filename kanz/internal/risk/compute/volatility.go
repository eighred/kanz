package compute

import (
	"context"
	"math"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// MODEL-01g wires a real volatility model into the RISK-08 uncertainty path.
//
// # The gap MODEL-01g closes
//
// RISK-08 built the propagation through measures (uncertainty.go) and exercised
// it in tests, but every real engine position carried
// MarketValueUncertainty=nil because no volatility model existed — so the
// api/v1 Measure.UncertaintyAbs was always nil in production. The historical
// market-data plane (MODEL-01b/c) now supplies return series; this file derives
// a per-instrument one-sigma return volatility from them and turns it into the
// per-position money band the propagation already knows how to sum.
//
// # The seam mirrors MODEL-01c / MODEL-01f
//
// A VolModel reads the same ReturnsProvider that feeds historical/Monte-Carlo
// VaR, so the uncertainty band and the VaR figure are estimated off one
// consistent data source. PopulateUncertainty is the engine-side enrichment:
// given a portfolio *snapshot* it fills each position's MarketValueUncertainty
// before compute runs — the reference mirror of factor.Classifier feeding
// exposure bucketing. The pure MeasureFunc / ComputeMeasures contract is
// untouched; the band enters through the snapshot, not the call site.
//
// # Point-in-time correctness
//
// Volatility is estimated as of the portfolio's state time (p.AsOf()), so the
// provider bounds both the observation window and the bitemporal knowledge
// horizon there — a recompute or backtest sees only the returns observable and
// known then (the MODEL-01i no-future-leakage property).

// DefaultVolMinObs is the minimum number of returns required to estimate a
// volatility. Below it the model reports ok=false and the caller leaves that
// position's uncertainty nil rather than publishing a band off a handful of
// points (which the sample stddev would estimate far too noisily to trust).
const DefaultVolMinObs = 20

// VolModel supplies an instrument's one-sigma periodic return volatility as of a
// knowledge horizon — the fractional (not money) standard deviation of its
// returns. Point-in-time via asOf, the reference mirror of the MODEL-01c
// ReturnsProvider.
type VolModel interface {
	// Volatility returns the one-sigma return volatility for instrumentID as of
	// asOf. ok=false when there is insufficient history to estimate it (the
	// caller leaves that position's uncertainty nil rather than fabricating a
	// zero band). An error is a genuine data-plane failure — the caller skips
	// the position so a transient store fault degrades to "no uncertainty"
	// rather than failing the whole recompute.
	Volatility(ctx context.Context, instrumentID string, asOf time.Time) (sigma float64, ok bool, err error)
}

// ReturnsVolModel estimates volatility as the sample standard deviation of an
// instrument's historical return series, read from a ReturnsProvider (the
// MODEL-01b/c market-data plane). It is the concrete VolModel the engine wires;
// because it shares the returns that drive VaR, the uncertainty band and the
// VaR estimate stay consistent.
type ReturnsVolModel struct {
	rp     ReturnsProvider
	window int
	minObs int
}

// NewReturnsVolModel builds a model over rp. A non-positive window takes the
// historical-returns default (DefaultReturnWindow); minObs takes
// DefaultVolMinObs.
func NewReturnsVolModel(rp ReturnsProvider, window int) *ReturnsVolModel {
	if window <= 0 {
		window = DefaultReturnWindow
	}
	return &ReturnsVolModel{rp: rp, window: window, minObs: DefaultVolMinObs}
}

// Volatility implements VolModel via the sample standard deviation of the
// instrument's returns.
func (m *ReturnsVolModel) Volatility(ctx context.Context, instrumentID string, asOf time.Time) (float64, bool, error) {
	rets, err := m.rp.Returns(ctx, instrumentID, asOf, m.window)
	if err != nil {
		return 0, false, err
	}
	if len(rets) < m.minObs {
		return 0, false, nil
	}
	return sampleStdDev(rets), true, nil
}

// sampleStdDev returns the unbiased (n-1) sample standard deviation of xs, or 0
// for fewer than two points.
func sampleStdDev(xs []float64) float64 {
	n := len(xs)
	if n < 2 {
		return 0
	}
	var mean float64
	for _, x := range xs {
		mean += x
	}
	mean /= float64(n)
	var ss float64
	for _, x := range xs {
		d := x - mean
		ss += d * d
	}
	return math.Sqrt(ss / float64(n-1))
}

// PopulateUncertainty fills each position's MarketValueUncertainty from the vol
// model: the one-sigma money band is |MarketValue| × σ_return, the delta-normal
// approximation (a position's value moves ~proportionally to its instrument's
// return). It mutates p in place, so it MUST be handed a snapshot (the engine's
// Store.Snapshot clone), never the live portfolio.
//
// A position is left nil ("no uncertainty propagated") when its MarketValue is
// nil, the model has insufficient history (ok=false), or the model errors —
// matching the RISK-08 nil-as-absent convention end-to-end. A nil vm is a no-op,
// so an engine wired without a market-data plane behaves exactly as before
// MODEL-01g (every band nil).
func PopulateUncertainty(ctx context.Context, p *domain.Portfolio, vm VolModel) {
	if vm == nil {
		return
	}
	asOf := p.AsOf()
	for _, pos := range p.Positions() {
		if pos.MarketValue == nil {
			continue
		}
		sigma, ok, err := vm.Volatility(ctx, string(pos.InstrumentID), asOf)
		if err != nil || !ok {
			continue
		}
		pos.MarketValueUncertainty = scaleMoneyByVol(pos.MarketValue, sigma)
		p.SetPosition(pos)
	}
}

// scaleMoneyByVol returns |m| × |sigma| in m's currency at uncertaintyExp
// precision — the money one-sigma band. Goes through float64 (sigma is a
// float estimate) which is acceptable for an uncertainty band, not for the
// money value itself (see decimalToFloat's note).
func scaleMoneyByVol(m *commonpb.Money, sigma float64) *commonpb.Money {
	band := math.Abs(decimalToFloat(m.Amount)) * math.Abs(sigma)
	return &commonpb.Money{
		Amount:       floatToDecimal(band, uncertaintyExp),
		CurrencyCode: m.CurrencyCode,
	}
}

// Compile-time assertion that ReturnsVolModel satisfies VolModel.
var _ VolModel = (*ReturnsVolModel)(nil)

// Package varmodel holds the real Value-at-Risk models that replace the RISK-07
// 1%×gross placeholder. Each model is a compute.ReturnsMeasure (MODEL-01c) — it
// reads historical returns through the injected provider and is registered over
// compute.MeasureVaR99 via compute.BindReturns, so the RISK-07 Registry, the
// api/v1 contract, and every caller stay unchanged (only the measure body does).
//
// The package directory is `var`; the package identifier is `varmodel` because
// `var` is a Go keyword. MODEL-01d ships historical simulation here; MODEL-01e
// adds Monte-Carlo alongside it.
//
// # Why the placeholder is not deleted
//
// compute.VaR99 (1%×gross) remains the compute.DefaultRegistry entry: varmodel
// imports compute, so compute cannot import varmodel (cycle) and the default,
// data-free registry cannot hold a provider-backed model. The placeholder is
// therefore the honest no-market-data fallback; an engine WITH a price store
// calls Register to override MeasureVaR99 with historical simulation. That
// override is where the RISK-12 `VaR99/Gross=0.01` property stops holding —
// failure-as-documentation that VaR is no longer the placeholder
// (TestHistorical_DecouplesFromGrossPlaceholder pins the new reality).
package varmodel

import (
	"context"
	"math"
	"sort"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// DefaultConfidence is the VaR confidence level — 99% one-day, matching the
// MeasureVaR99 name.
const DefaultConfidence = 0.99

// varExponent is the Decimal scale of the emitted VaR value: cents. VaR is a
// money loss in the portfolio base currency; the inputs (returns) are
// statistics, so the result is rounded to a money scale on the way out.
const varExponent int32 = -2

// Config parameterizes a VaR model.
type Config struct {
	// Confidence is the VaR confidence level in (0,1). Zero ⇒ DefaultConfidence.
	Confidence float64
	// Window is the historical lookback (number of return scenarios) passed to
	// the provider. Zero ⇒ the provider's own default.
	Window int
	// Draws is the number of Monte-Carlo simulations (MonteCarlo only; ignored by
	// Historical). Zero ⇒ DefaultDraws.
	Draws int
	// Seed seeds the Monte-Carlo RNG (MonteCarlo only). A fixed seed makes VaR
	// reproducible across recomputes/replay — deterministic given inputs + seed.
	// Zero ⇒ DefaultSeed.
	Seed int64
}

func (c Config) confidence() float64 {
	if c.Confidence <= 0 || c.Confidence >= 1 {
		return DefaultConfidence
	}
	return c.Confidence
}

// Historical builds a historical-simulation VaR measure: it revalues the
// portfolio under each past return scenario, forms the empirical P&L
// distribution, and reports the loss at the configured confidence level.
//
// # Method
//
// For every position (in the portfolio base currency — the RISK-07 same-
// currency convention; cross-currency needs an FX layer this package does not
// have), it pulls the instrument's return series via the provider and forms
// per-scenario P&L = Σ_i value_i × return_i. Scenarios are tail-aligned to the
// shortest available series (the providers return most-recent-N, so the recent
// tail lines up; exact date alignment is a MODEL-01b refinement once the
// provider returns date-tagged returns). VaR_α = −quantile(P&L, 1−α), floored
// at zero (a non-loss quantile ⇒ no VaR). Insufficient data yields a zero-value
// measure, never an error — the MeasureFunc contract; the response layer marks
// it degraded.
func Historical(cfg Config) compute.ReturnsMeasure {
	conf := cfg.confidence()
	window := cfg.Window
	return func(ctx context.Context, p *domain.Portfolio, rp compute.ReturnsProvider) v1.Measure {
		base := string(p.BaseCurrency())
		type leg struct {
			value   float64
			returns []float64
		}
		var legs []leg
		minLen := -1
		for _, pos := range p.Positions() {
			if pos.MarketValue == nil || pos.MarketValue.CurrencyCode != base {
				continue
			}
			r, err := rp.Returns(ctx, string(pos.InstrumentID), p.AsOf(), window)
			if err != nil || len(r) == 0 {
				continue
			}
			legs = append(legs, leg{value: decimalToFloat(pos.MarketValue.Amount), returns: r})
			if minLen < 0 || len(r) < minLen {
				minLen = len(r)
			}
		}
		if len(legs) == 0 || minLen < 2 {
			return zeroMeasure()
		}

		pnl := make([]float64, minLen)
		for _, lg := range legs {
			off := len(lg.returns) - minLen // tail-align to the common window
			for t := 0; t < minLen; t++ {
				pnl[t] += lg.value * lg.returns[off+t]
			}
		}
		sort.Float64s(pnl)

		loss := -quantile(pnl, 1-conf)
		if loss < 0 {
			loss = 0
		}
		return v1.Measure{
			Name:  compute.MeasureVaR99,
			Value: floatToDecimal(loss, varExponent),
		}
	}
}

// Register overrides MeasureVaR99 in r with historical-simulation VaR closed
// over provider. The engine calls this at startup once it has a price-store
// provider; absent that, r keeps compute.VaR99 (the placeholder).
func Register(ctx context.Context, r *compute.Registry, provider compute.ReturnsProvider, cfg Config) {
	r.Register(compute.MeasureVaR99, compute.BindReturns(ctx, provider, Historical(cfg)))
}

// quantile is the empirical α-quantile of an ascending-sorted sample, with
// linear interpolation between order statistics (the R-7 / NumPy default).
func quantile(sorted []float64, q float64) float64 {
	n := len(sorted)
	switch {
	case n == 0:
		return 0
	case n == 1:
		return sorted[0]
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[n-1]
	}
	h := float64(n-1) * q
	lo := int(math.Floor(h))
	if lo+1 >= n {
		return sorted[n-1]
	}
	return sorted[lo] + (h-float64(lo))*(sorted[lo+1]-sorted[lo])
}

func zeroMeasure() v1.Measure {
	return v1.Measure{Name: compute.MeasureVaR99, Value: &commonpb.Decimal{Coefficient: 0, Exponent: 0}}
}

// decimalToFloat / floatToDecimal are the local Decimal⇄float bridge (compute's
// equivalents are package-private). Returns/VaR are float-domain statistics; the
// emitted Value is rounded back to a money-scale Decimal.
func decimalToFloat(d *commonpb.Decimal) float64 {
	if d == nil {
		return 0
	}
	return float64(d.Coefficient) * math.Pow10(int(d.Exponent))
}

func floatToDecimal(f float64, exp int32) *commonpb.Decimal {
	scaled := f * math.Pow10(int(-exp))
	return &commonpb.Decimal{Coefficient: int64(math.Round(scaled)), Exponent: exp}
}

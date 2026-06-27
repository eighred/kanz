package performance

import "math"

// Ex-post (realized) risk statistics (PERF-01e): tracking error, information
// ratio, Sharpe ratio, and benchmark beta from a return series. These summarize
// how the portfolio behaved relative to its benchmark and a risk-free rate —
// the risk-adjusted half of "how did we do".

// RiskStats are the ex-post risk statistics over a return series. The Go-native
// shape of performance.v1.RiskStatistics.
type RiskStats struct {
	TrackingError    float64
	InformationRatio float64
	Sharpe           float64
	Beta             float64
	Periods          int
}

// ExPostRisk computes the statistics from per-period portfolio and benchmark
// returns (aligned, same length) given a per-period risk-free rate and the
// number of periods per year (252 daily, 52 weekly, 12 monthly) for
// annualization:
//
//	tracking error    = stdev(rp − rb)·√periodsPerYear
//	information ratio  = mean(rp − rb)/stdev(rp − rb)·√periodsPerYear
//	Sharpe             = mean(rp − rf)/stdev(rp)·√periodsPerYear
//	beta               = cov(rp, rb)/var(rb)
//
// Fewer than two aligned observations ⇒ zero-value stats (variance is undefined).
// A zero denominator (no active variance / no portfolio variance / no benchmark
// variance) leaves that ratio 0 rather than ±Inf.
func ExPostRisk(portfolio, benchmark []float64, riskFreePerPeriod, periodsPerYear float64) RiskStats {
	n := len(portfolio)
	if n < 2 || len(benchmark) != n {
		return RiskStats{Periods: n}
	}
	active := make([]float64, n)
	excess := make([]float64, n)
	for i := range portfolio {
		active[i] = portfolio[i] - benchmark[i]
		excess[i] = portfolio[i] - riskFreePerPeriod
	}
	annual := math.Sqrt(periodsPerYear)

	teRaw := stdev(active)
	pVol := stdev(portfolio)
	bVar := variance(benchmark)

	stats := RiskStats{Periods: n, TrackingError: teRaw * annual}
	if teRaw > 0 {
		stats.InformationRatio = mean(active) / teRaw * annual
	}
	if pVol > 0 {
		stats.Sharpe = mean(excess) / pVol * annual
	}
	if bVar > 0 {
		stats.Beta = covariance(portfolio, benchmark) / bVar
	}
	return stats
}

// mean is the arithmetic mean.
func mean(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// variance is the Bessel-corrected sample variance.
func variance(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := mean(xs)
	var s float64
	for _, x := range xs {
		d := x - m
		s += d * d
	}
	return s / float64(len(xs)-1)
}

// stdev is the sample standard deviation.
func stdev(xs []float64) float64 { return math.Sqrt(variance(xs)) }

// covariance is the Bessel-corrected sample covariance of two equal-length
// series.
func covariance(xs, ys []float64) float64 {
	if len(xs) < 2 || len(xs) != len(ys) {
		return 0
	}
	mx, my := mean(xs), mean(ys)
	var s float64
	for i := range xs {
		s += (xs[i] - mx) * (ys[i] - my)
	}
	return s / float64(len(xs)-1)
}

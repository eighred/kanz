// Package indicator computes technical indicators over an OHLCV series (#416 C1).
//
// # Why this exists, and what it is not
//
// The platform could not answer "are we buying at the right time" because nothing
// in the tree evaluated a decision — a tree-wide grep for rsi|macd|bollinger
// returned three hits, all string labels in Python test fixtures. #416's owner
// ruling of 2026-08-12 retired the out-of-repo alpha rule; this is the C1 layer
// that ruling depends on, and it is deliberately the DUMB half: pure functions
// from a price series to a number.
//
// IT DECIDES NOTHING. No thresholds, no signals, no "RSI below 30 is a buy". An
// indicator that carried its own trading rule would put strategy in the one place
// that has no backtest, and every consumer would inherit whichever threshold the
// first caller happened to want. Engines compose these; this package has no
// opinion.
//
// # Insufficient history returns ok=false, never a number
//
// Every function here returns (value, ok). A 14-period RSI over 9 bars has no
// answer, and the tempting alternatives are all worse: 0 is a valid RSI meaning
// "sold off hard", 50 is a valid one meaning "balanced", and NaN propagates
// silently through arithmetic until it surfaces somewhere unrelated. Each of
// those makes "not enough history" indistinguishable from a real reading, which
// is the failure this estate refuses everywhere else.
//
// It matters most exactly where it is least visible. A backtest walks forward
// from the first bar, so the earliest evaluations are always the ones with the
// least history — and a library that returned a plausible number there would
// make every strategy's opening trades look like decisions instead of artifacts.
//
// # TWO SMOOTHING CONVENTIONS, AND THEY ARE NOT INTERCHANGEABLE
//
// This is the trap in every indicator library, and it produces numbers that look
// right:
//
//	EMA, MACD          alpha = 2/(n+1)      "modern"/exponential
//	RSI, ATR           alpha = 1/n          WILDER'S
//
// Wilder defined RSI and ATR with 1/n. Using 2/(n+1) for them yields a series
// with the same shape, the same bounds and the same turning points, differing by
// a few percent — so it passes every eyeball check and every property test, and
// disagrees with every chart the trader is looking at. The two are separate
// functions here (ema and wilder) rather than one function with a parameter,
// because a parameter is a thing a caller can get wrong.
//
// # SEEDING, which is the other way to be plausibly wrong
//
// A recursive average has to start somewhere. Seeding it with the first
// observation (e := xs[0]) is the common shortcut and it is wrong for roughly 3n
// bars — the seed's weight decays but does not vanish, so early values carry the
// first print's noise. Every recursion here seeds with the SIMPLE MEAN of the
// first n, which is what Wilder specified and what charting packages do, and the
// functions refuse to answer at all until they have that many.
//
// # Floats, not Decimal
//
// The estate's money rule is that quantities are common.v1.Decimal, and it holds
// for values of record. An indicator is a float-derived analytic like the Greeks
// — compute.OptionSpec calls itself "the float working shape" for the same
// reason — and it is never a value of record: nothing settles against an RSI.
// The Decimal→float conversion happens once, at the store boundary in Source.
package indicator

import "math"

// Series functions take the price history OLDEST FIRST and answer for its most
// recent point. That order is the store's own (BarQuery returns bars ordered by
// BucketStart ascending), so no caller has to reverse anything — a reversed
// series is the kind of bug that produces a perfectly plausible indicator
// pointing the wrong way.

// SMA is the simple moving average of the last n values.
func SMA(xs []float64, n int) (float64, bool) {
	if n <= 0 || len(xs) < n {
		return 0, false
	}
	var sum float64
	for _, x := range xs[len(xs)-n:] {
		sum += x
	}
	return sum / float64(n), true
}

// EMA is the exponential moving average with alpha = 2/(n+1), seeded with the
// SMA of the first n values.
//
// It consumes the WHOLE series, not the last n: that is what makes it
// exponential. A caller that slices to the last n first gets a number that is
// neither an EMA nor an SMA.
func EMA(xs []float64, n int) (float64, bool) {
	if n <= 0 || len(xs) < n {
		return 0, false
	}
	return ema(xs, n), true
}

func ema(xs []float64, n int) float64 {
	e := mean(xs[:n])
	alpha := 2.0 / (float64(n) + 1.0)
	for _, x := range xs[n:] {
		e = alpha*x + (1-alpha)*e
	}
	return e
}

// wilder is Wilder's smoothing, alpha = 1/n, seeded with the mean of the first n.
// Used by RSI and ATR and by nothing else — see the package doc.
func wilder(xs []float64, n int) float64 {
	e := mean(xs[:n])
	for _, x := range xs[n:] {
		e = (e*float64(n-1) + x) / float64(n)
	}
	return e
}

// RSI is Wilder's Relative Strength Index over n periods, in [0,100].
//
// It needs n+1 prices, not n: the input is n CHANGES, and a change needs two
// prices. An implementation that asks for n is off by one bar forever, which is
// invisible in the value and visible in every comparison to a chart.
func RSI(xs []float64, n int) (float64, bool) {
	if n <= 0 || len(xs) < n+1 {
		return 0, false
	}
	gains := make([]float64, len(xs)-1)
	losses := make([]float64, len(xs)-1)
	for i := 1; i < len(xs); i++ {
		switch d := xs[i] - xs[i-1]; {
		case d > 0:
			gains[i-1] = d
		default:
			losses[i-1] = -d
		}
	}
	avgLoss := wilder(losses, n)
	// NO DOWN MOVES AT ALL is RSI 100 by definition, not a division by zero and
	// not a missing answer. A series that only rose is a real market state — it
	// is what a limit-up move looks like — and returning ok=false there would
	// make the one reading a momentum strategy most wants disappear.
	if avgLoss == 0 {
		return 100, true
	}
	rs := wilder(gains, n) / avgLoss
	return 100 - 100/(1+rs), true
}

// MACDValue is the three series a MACD reading carries.
type MACDValue struct {
	// MACD is fastEMA − slowEMA.
	MACD float64
	// Signal is the EMA of the MACD line itself, so it needs the MACD's own
	// history rather than the price's.
	Signal float64
	// Histogram is MACD − Signal, the crossover distance.
	Histogram float64
}

// MACD computes the classic (12,26,9) triple when called with those periods.
//
// The signal line is an EMA OF THE MACD LINE, which is why this cannot be
// assembled from two EMA calls by a caller: it needs the MACD line's history,
// which means recomputing both EMAs at every point. Doing that here once is the
// difference between one implementation and one per caller.
func MACD(xs []float64, fast, slow, signal int) (MACDValue, bool) {
	if fast <= 0 || slow <= 0 || signal <= 0 || fast >= slow {
		return MACDValue{}, false
	}
	// The MACD line starts once the SLOW EMA can be computed, and the signal
	// needs `signal` points of that line.
	if len(xs) < slow+signal-1 {
		return MACDValue{}, false
	}
	line := make([]float64, 0, len(xs)-slow+1)
	for i := slow; i <= len(xs); i++ {
		line = append(line, ema(xs[:i], fast)-ema(xs[:i], slow))
	}
	m := line[len(line)-1]
	s := ema(line, signal)
	return MACDValue{MACD: m, Signal: s, Histogram: m - s}, true
}

// Band is a Bollinger reading.
type Band struct {
	Middle, Upper, Lower float64
	// Width is (Upper−Lower)/Middle — the normalised band width, which is the
	// part that compares across instruments. A raw width of 12 means nothing
	// without knowing whether the price is 40 or 40,000.
	Width float64
}

// Bollinger is the n-period SMA with bands k population standard deviations out.
//
// POPULATION, not sample. Bollinger's own definition uses the population
// deviation over the window, and the difference at n=20 is a factor of
// sqrt(20/19) ≈ 1.026 — under 3% on the band width, which is exactly the size
// that never looks wrong and never matches the chart.
func Bollinger(xs []float64, n int, k float64) (Band, bool) {
	mid, ok := SMA(xs, n)
	if !ok {
		return Band{}, false
	}
	w := xs[len(xs)-n:]
	var ss float64
	for _, x := range w {
		ss += (x - mid) * (x - mid)
	}
	sd := math.Sqrt(ss / float64(n))
	b := Band{Middle: mid, Upper: mid + k*sd, Lower: mid - k*sd}
	if mid != 0 {
		b.Width = (b.Upper - b.Lower) / mid
	}
	return b, true
}

// ATR is Wilder's Average True Range over n periods.
//
// True range is max(high−low, |high−prevClose|, |low−prevClose|). The two terms
// involving the previous close are what make it a GAP-AWARE range: an instrument
// that opens far from yesterday's close has moved, and a plain high−low says it
// did not. On a 24/7 crypto venue gaps are rarer than on an equity open, which is
// the reason to keep the terms rather than to drop them — the one time they
// matter is a venue halt or an outage, which is precisely when a risk-sizing
// input must not understate.
//
// # THE FIRST BAR HAS NO PREVIOUS CLOSE, and the convention here is Wilder's
//
// Its true range is therefore taken as the plain high−low. That is what Wilder
// specified and what charting packages compute, and it is NOT the only defensible
// choice: the first bar's range is missing two of the three terms by
// construction, so an implementation that skipped it and started at index 1 would
// be averaging only comparable quantities.
//
// This was written that stricter way first, and the exact-value test caught it —
// against a 60-bar fixture the two differ by 0.1% (5.5684 against 5.5626). The
// difference is a seed artifact and decays as the series grows, which is exactly
// what makes it the wrong thing to be quietly original about: it never looks
// wrong, and it disagrees with every chart the trader is comparing to. Matching
// the published definition is worth more than being locally tidier.
//
// It still requires n+1 bars rather than n, so at least one genuine true range
// reaches the seed average — a seed made entirely of plain ranges would be a
// range, not a true range.
func ATR(highs, lows, closes []float64, n int) (float64, bool) {
	if n <= 0 || len(highs) != len(lows) || len(highs) != len(closes) || len(closes) < n+1 {
		return 0, false
	}
	tr := make([]float64, 0, len(closes))
	tr = append(tr, highs[0]-lows[0]) // no previous close; see above
	for i := 1; i < len(closes); i++ {
		tr = append(tr, math.Max(highs[i]-lows[i],
			math.Max(math.Abs(highs[i]-closes[i-1]), math.Abs(lows[i]-closes[i-1]))))
	}
	return wilder(tr, n), true
}

// RealizedVol is the SAMPLE standard deviation of the last n log returns.
//
// Log returns, because they are additive across periods and symmetric: a +10%
// then −10% round trip is zero in logs and −1% in simple returns, so a simple
// series drifts against a mean-reverting price. SAMPLE deviation (n−1), because
// this estimates the population from a window — matching internal/risk's own
// volatility convention, which matters because a sizing rule that mixed the two
// would disagree with the risk engine about the same instrument.
//
// It is NOT annualised. The scaling factor depends on the bar resolution, which
// this function cannot see, and a wrong sqrt(periods) is a silent factor of 20+.
func RealizedVol(xs []float64, n int) (float64, bool) {
	if n <= 1 || len(xs) < n+1 {
		return 0, false
	}
	rets := make([]float64, 0, n)
	for i := len(xs) - n; i < len(xs); i++ {
		if xs[i-1] <= 0 || xs[i] <= 0 {
			// A NON-POSITIVE PRICE HAS NO LOG RETURN. It is bad data rather than a
			// market state, and inventing a return for it would put a fabricated
			// number into a sizing input.
			return 0, false
		}
		rets = append(rets, math.Log(xs[i]/xs[i-1]))
	}
	mu := mean(rets)
	var ss float64
	for _, r := range rets {
		ss += (r - mu) * (r - mu)
	}
	return math.Sqrt(ss / float64(n-1)), true
}

func mean(xs []float64) float64 {
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

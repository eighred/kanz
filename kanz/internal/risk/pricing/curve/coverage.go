package curve

// STRIP COVERAGE (#908) — what a calibrated curve was actually built FROM.
//
// A rate curve is calibrated from a strip of market instruments the deployment
// configures: a 3M deposit on the short end, a rate future in the belly, a 5Y
// and a 10Y par swap on the long end. The QuoteSource reads the latest quote
// for each and skips the ones it cannot use — an instrument that has never
// ticked, one that stopped (delisted, rolled, feed outage), one whose id is a
// typo in the reference spec, one quoting a garbage or non-positive mid.
// Skipping is right on its own: a calibration cannot use a price that does not
// exist, and refusing the whole currency for one dead instrument would take the
// curve out of service for a fixable data problem.
//
// WHAT WAS WRONG IS THAT SKIPPING WAS INVISIBLE. Calibrate refuses only an
// EMPTY set, so a nine-point strip that had lost its four longest tenors
// calibrated, published and served a five-pillar curve indistinguishable in
// every respect from one whose strip is five points long and complete. Past the
// last surviving pillar Curve.Zero extrapolates FLAT, so every discount factor,
// every DV01 and every valuation beyond that tenor was derived from a curve
// nobody could tell was short — a confidently wrong answer on the risk plane,
// internally consistent and unflagged. That is this repository's own rule
// broken: "nothing configured" and "checked, and fine" must never look the same.
//
// # The same doctrine as domain.v1.InputCoverage, over a different set
//
// InputCoverage records how much of the BOOK one measure was computed over —
// "the difference between a MEASURED zero and a CONFIDENT one". This is that
// doctrine one layer down, over the CALIBRATION STRIP: how much of the input a
// deployment declared its curve needs actually reached the fit.
//
// IT IS NOT v1.InputCoverage, deliberately, for the reasons
// internal/marketdata/store.Window states for its own case and two more:
//
//   - The two count different things. InputCoverage counts POSITIONS left out
//     of a measure; a missing calibration instrument leaves no position out of
//     anything — it changes the number every position gets. Reusing the type
//     would make Contributed mean "positions" in one place and "pillars" in
//     another, which is how a field stops being readable.
//   - Its exclusion list is a BOUNDED SAMPLE (MaxInputExclusions = 32) because a
//     book is unbounded. A calibration strip is bounded by this pod's own
//     configuration, so Missing below is COMPLETE — and completeness is the
//     whole point, since what an operator needs is the id of the instrument to
//     go and look at.
//   - internal/risk/pricing/curve is the pure constructor and evaluator; it
//     holds no I/O and no engine types. Pulling the risk API surface into it to
//     describe a market-data gap would invert what this package is.

// StripCoverage records how much of a currency's CONFIGURED calibration strip
// reached the calibration that produced a curve.
//
// PRESENCE IS THE SIGNAL, the same rule InputCoverage is a message rather than
// two scalars for. A zero value means "this source does not declare a strip" —
// it does NOT mean "nothing was quoted". Reported tells the two apart, and
// Curve.StripCoverage returns an ok the same way: a curve built by NewZeroCurve
// or Bootstrap reports NO coverage rather than reporting full coverage, because
// those constructors have no strip to be short of. That is the UNKNOWN third
// value, not a defaulted zero.
//
// NO DIRECTION CLAIM IS MADE, for the same reason InputCoverage makes none. A
// curve short of its long end does not price long-dated flows as zero — it
// prices them off a flat extrapolation of whatever pillar survived, which can
// be higher or lower than the truth. A caller gating on a valuation must read a
// non-zero len(Missing) as "this curve is not the curve the deployment
// configured", never as an annotation on a good number.
type StripCoverage struct {
	// Configured is how many instruments the deployment's reference spec names
	// for this currency. Zero means the source does not report coverage.
	Configured int
	// Quoted is how many of them had a usable price and reached the fit. It is
	// the number of quotes handed to Calibrate, which is the number of pillars
	// the resulting curve carries.
	Quoted int
	// Missing names every configured instrument that did NOT reach the fit, with
	// why. COMPLETE, not a sample: the strip is bounded by configuration, and
	// the identity is what lets somebody go and look at the one that died.
	Missing []MissingQuote
}

// MissingQuote names one configured calibration instrument that was not in the
// strip the calibration ran over.
type MissingQuote struct {
	// InstrumentID is the configured instrument's cache key — the same id the
	// reference spec names, so an operator can grep the deployment for it.
	InstrumentID string
	// Reason is drawn from the closed vocabulary below rather than being free
	// text, because it is also a metric label and a label must not be whatever a
	// future edit writes.
	Reason string
}

// The reasons a configured instrument is missing from the strip. Two rather
// than one because they have DIFFERENT OWNERS: no_quote is a question for the
// deployment's reference spec and the market-data spine (is the id right, is
// anything publishing it), while unusable_mid is a question for the venue
// feeding it (it is publishing, and what it publishes cannot be a rate).
const (
	// MissingNoQuote: the cache holds no market event for this instrument at
	// all. A typo in the reference spec, an instrument nothing publishes, or one
	// that stopped — the last-value cache simply never has an entry, so a
	// permanently mistyped id and a feed that died look identical here and are
	// told apart by whether the id appears on the spine.
	MissingNoQuote = "no_quote"
	// MissingUnusableMid: an event IS cached and carries no usable rate — no
	// bid, ask, last or close, or a mid that is zero or negative. The instrument
	// is ticking; what it ticks cannot be calibrated from.
	MissingUnusableMid = "unusable_mid"
)

// Reported says whether this coverage was stated at all. False is the UNKNOWN
// value: the source declared no strip, so nothing here may be read as "the
// whole strip was quoted".
func (c StripCoverage) Reported() bool { return c.Configured > 0 }

// Complete reports that every configured instrument reached the fit. It is
// false for an unreported coverage — an unknown is not a completeness claim.
func (c StripCoverage) Complete() bool { return c.Reported() && c.Quoted >= c.Configured }

// clone deep-copies the coverage so a curve cannot alias its source's slice.
func (c StripCoverage) clone() StripCoverage {
	out := c
	if c.Missing != nil {
		out.Missing = make([]MissingQuote, len(c.Missing))
		copy(out.Missing, c.Missing)
	}
	return out
}

// Strip is one currency's calibration input as the source resolved it: the
// quotes that were usable, together with how much of the configured set they
// are. The two travel as ONE value because they are one answer — a caller that
// can take the quotes without the coverage is the caller this issue is about.
type Strip struct {
	// Quotes are the usable calibration instruments, in no particular order.
	Quotes []RateQuote
	// Coverage says what Quotes is a subset OF. Its zero value is the
	// "unreported" case (see StripCoverage).
	Coverage StripCoverage
}

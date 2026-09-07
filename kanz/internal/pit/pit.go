// Package pit is the estate's one implementation of point-in-time version
// retention: an ascending list of versions per key, pruned to an explicit
// horizon measured from the NEWEST RETAINED element, releasing what it drops.
// The risk pricing stores — curve (rates), credit (hazard) and volsurface
// (implied vol) — each keep that container, and #811 is what happens when only
// the retain half of the concept is decided.
//
// # WHY IT SITS AT internal/pit AND NOT UNDER internal/risk/pricing (#871)
//
// It was written under internal/risk/pricing/ for the three stores that
// motivated it. #867 then added internal/marketedge/volprofile, whose retained
// completed-session list is the same container, and it could not import this
// package: test/arch/risk_boundary_test.go admits only internal/risk/api/v* from
// outside the risk module, and the market-data edge depending on the risk
// pricing tree is the inversion store.Window's doc already refuses. So the shape
// was written a second time, carrying its own copy of #862's fix — the
// copied-helper failure mode AGENTS.md names by cost, and the reason the next
// defect of that class would have had to be fixed twice.
//
// The package itself never had a risk dependency (sort and time), so the
// ownership claim was the PATH and nothing else. AGENTS.md's rule — shared code
// is promoted out of a service's internal/ when a SECOND consumer appears — is
// what moved it here. test/arch/one_horizon_prune_test.go is the guard that
// keeps a third copy from appearing quietly.
//
// # What was decided, and what was not (#811)
//
// The three stores were written point-in-time on purpose: a risk number is
// reproduced AS OF an instant, so a read at T must resolve the curve that was
// live at T and never a later one. Every store's doc comment argues that case
// and every one of them is right. None of them says for HOW LONG, and
// `grep -rn "delete(" internal/risk/pricing` returned nothing: each calibration
// refresh appended a version and nothing ever removed one, so retention grew
// with UPTIME rather than with anything financial. At the one-minute intraday
// cadence the risk-engine composition root can be configured with, that is
// ~525k retained curves per currency per year in a process that never drops
// one.
//
// # Why a horizon is safe here, and what it is NOT
//
// THESE STORES ARE IN-MEMORY AND START EMPTY. Nothing persists them, nothing
// replays them, and a pod restart (a deploy, a KEDA scale event, an OOM kill)
// leaves them holding no versions at all. So the depth a read can ever reach is
// already bounded by process uptime, and a read for an as-of older than the
// process start ALREADY resolves ok=false — the "predates the first version"
// arm every store has had since it was written.
//
// That is the fact that makes a horizon a bound on an already-bounded thing
// rather than a decision to destroy audit evidence. Reproducing what the book
// was worth on the 3rd is a durable-storage question and this store is not the
// answer to it — it cannot be, because it does not survive to the 4th. What the
// store genuinely provides is a SHORT point-in-time window: a portfolio's as-of
// is the timestamp of the newest event applied to it, so a valuation commonly
// runs slightly behind the newest calibration and must not silently pick up a
// curve from after the state it is pricing.
//
// # The horizon is measured from the newest RETAINED version, not from now
//
// Deliberately, and it is the difference between degrading and breaking. A
// wall-clock horizon would empty a store whose calibration feed has stalled,
// turning "the curve is stale" into "there is no curve" — and a missing curve
// is a critical unknown that fails closed: no discount curve means no DV01,
// which means a bond book reports zero rate risk, which is what a limit is
// checked against. Anchoring on the newest retained version means a stalled
// store keeps serving its last curve exactly as it does today, and staleness
// stays the separate, already-observable problem it is.
package pit

import (
	"sort"
	"time"
)

// DefaultHorizon is how far back the pricing stores retain versions when the
// composition root does not say otherwise.
//
// SEVEN DAYS, AND THE NUMBER IS NOT NEW. It is spotsource.DefaultMaxAge — the
// one age bound this repository has already decided, argued and sourced for a
// valuation input measured against the valuation's own as-of. That constant
// refuses a mark older than a week at the requested asOf, and its derivation
// (internal/risk/spotsource/provider.go) is the longest routine gap between
// daily closes — Good Friday plus Easter Monday, five calendar days — rounded
// up to a week.
//
// Matching it is what keeps the two halves of one valuation consistent. A
// curve retained BEYOND the mark max-age could only ever be paired with a mark
// the system already refuses, so the extra retention buys no answerable
// question; a horizon SHORTER than it would mean a valuation whose mark is
// admissible and whose discount curve has been dropped, which is a DV01 of zero
// on a book that holds bonds. One number, one rationale, two stores.
//
// WHAT DECIDES THE READ SIDE. Every read of these stores in internal/risk —
// compute.FIProviders.Curve behind the FI measures, compute.GreeksProviders.Vol
// behind the greek measures, the structured measures,
// compute.BondRevaluer.RevalueBond and compute.Revaluer.RevalueOption — is
// handed domain.Portfolio.AsOf(), the timestamp of
// the newest event applied to that portfolio, advanced monotonically by
// Portfolio.SetPosition and Portfolio.SetAggregate and never set by a caller.
// query.v1's ExposureRequest.as_of / MeasuresRequest.as_of are carried across
// every hop and then ignored by the only engine implementation, so no query
// API, MCP tool, scheduled report or backtest in this tree asks these stores
// for an operator-chosen historical instant. The gap a horizon has to cover is
// therefore the lag between a portfolio's last event and the valuation pricing
// it — the same gap spotsource sized.
//
// RAISING IT IS AN OPERATOR DECISION THAT MOVES TOGETHER. spotsource's own
// comment names the case its week does not clear: Lunar New Year and Golden
// Week close their venues for 9-10 calendar days, and an operator on those
// venues raises the mark bound on the evidence the unresolved observer gives
// them. Whoever raises that must raise this horizon with it, or the mark comes
// back admissible and the curve that priced it is gone — which is why both are
// configurable and neither is a literal buried in a store.
//
// AND WHAT THIS IS NOT. It is not a claim that a week of valuations is
// reproducible. infra/dr/postgres/cluster.yaml says in its own words that
// long-horizon regulatory retention is the lakehouse's job and that nothing in
// this repository states a years-scale number; that stance is why a horizon
// here costs no audit evidence. See the package doc: a restart takes this store
// to zero, so no horizon could make it the book of record.
const DefaultHorizon = 7 * 24 * time.Hour

// MaxVersionsPerKey is the retained-version ceiling a composition root checks
// its configured horizon and refresh cadence against BEFORE starting, so a
// cadence too fast for the horizon is a refusal to start rather than a pod that
// grows until it is killed. Enforced at configuration time and not here on
// purpose: a cap applied silently at write time would shorten the horizon
// without saying so, and "nothing configured" and "checked, and fine" would
// look the same again.
const MaxVersionsPerKey = 20_000

// Version is one published value and the instant it became effective. The
// stores hold []Version[T] per key, ascending by AsOf.
type Version[T any] struct {
	AsOf time.Time
	V    T
}

// Put inserts v at asOf into the ascending version list and drops every version
// older than horizon measured back from the newest version retained, returning
// the new list and how many it dropped.
//
// Versions may arrive out of order (a nightly-close backfill landing behind an
// intraday refresh); the list stays sorted. A second Put at the same as-of
// replaces. A horizon of zero or less retains everything — the callers all pass
// a validated positive horizon, and the stores do not expose a way to reach
// this branch from configuration.
//
// The newest version is always retained (it cannot be older than itself minus a
// non-negative horizon), so a store that has stopped being refreshed keeps
// answering with its last value rather than going dark.
//
// A dropped count > 0 for an INSERT at the tail is the ordinary steady state. A
// dropped count of 1 for the version just inserted means a backfill arrived
// older than the horizon and was not retained; that is data loss the caller is
// told about rather than a silent no-op.
func Put[T any](vs []Version[T], asOf time.Time, v T, horizon time.Duration) ([]Version[T], int) {
	i := sort.Search(len(vs), func(i int) bool { return !vs[i].AsOf.Before(asOf) })
	if i < len(vs) && vs[i].AsOf.Equal(asOf) {
		vs[i].V = v
	} else {
		vs = append(vs, Version[T]{})
		copy(vs[i+1:], vs[i:])
		vs[i] = Version[T]{AsOf: asOf, V: v}
	}
	if horizon <= 0 || len(vs) == 0 {
		return vs, 0
	}
	cutoff := vs[len(vs)-1].AsOf.Add(-horizon)
	drop := sort.Search(len(vs), func(i int) bool { return !vs[i].AsOf.Before(cutoff) })
	if drop == 0 {
		return vs, 0
	}
	// THE HORIZON BOUNDS len, AND WITHOUT THIS IT DOES NOT BOUND THE HEAP (#862).
	//
	// vs[drop:] reslices the SAME backing array. Go keeps an entire array alive
	// while any slice references any part of it, so the dropped Version[T] values
	// — and the curve, credit or vol surface each one carries — stay reachable
	// from array[0:drop] and are NOT collected. They are released only when a
	// later append exceeds cap and reallocates, which for a store in steady state
	// is many refreshes away.
	//
	// The effect is a store that reports the right version COUNT and holds twice
	// the payloads: at the shipped 7-day horizon and a one-minute cadence, ~10,080
	// live versions per key with up to ~10,080 dead ones resident beside them, on
	// the risk engine, whose reason for existing is to hold a valuation surface in
	// memory. Every retention assertion in this package is on len(vs) or on what
	// At resolves; both are correct and both are blind to it.
	//
	// clear zeroes each Version[T], which drops the reference the array was
	// holding. It is O(drop), and drop is 1 in the steady state — the prune
	// removes one version per refresh once the horizon is full — so the ordinary
	// cost is a single struct write.
	return DropOldest(vs, drop), drop
}

// DropOldest releases the first `drop` elements of an ascending list and returns
// the remainder. It is the "releasing what it drops" half of this package's
// concept, named so it can be shared (#880).
//
// # Why this is the half worth sharing
//
// The reslice is the obvious part and the clear is the part that gets forgotten.
// #862 found exactly that inside Put — the horizon bounded the version COUNT and
// not the heap — and #867's volprofile then reimplemented the container and had
// to carry its own copy of the same clear(). The estate has already paid twice
// for this three-line act being written rather than called.
//
// internal/marketedge/trades is the third caller. Its prune compacted with
// `append(t.trades[:0], t.trades[i:]...)`, copying every live element down on
// EVERY print — O(n) per trade under the tape's write lock, on the market-data
// ingest path.
//
// THAT ONE WAS NOT A LEAK, and #880's second claim is worth correcting rather
// than repeating: the slots left past len hold DUPLICATES of elements that are
// still live, and the next append writes over them. Measured, not reasoned —
// after a copy-down prune the backing array reads [2 3 4 5 6 | 6 0 0] and the
// following append makes it [2 3 4 5 6 | 7 0 0].
//
// The clear below is what makes the RESLICE safe, which is why it is
// load-bearing for every caller of this function including the tape now: a
// reslice does not overwrite anything, so without it the dropped values sit
// behind the returned slice for as long as the array lives.
//
// # What it deliberately does not decide
//
// WHERE THE CUTOFF COMES FROM, and how the drop index is found. Put searches a
// sorted list with sort.Search; the trade tape scans linearly because it is
// ordered by ARRIVAL and a late print must not be treated as sorted. Those are
// genuine differences between a calibration store and a trade tape, and folding
// them together would either impose sortedness the tape does not have or add an
// accessor indirection to the hot path. What is one implementation here is the
// RELEASE, which is the part that has broken twice.
func DropOldest[T any](xs []T, drop int) []T {
	if drop <= 0 {
		return xs
	}
	if drop >= len(xs) {
		drop = len(xs)
	}
	// clear zeroes each element, dropping the reference the backing array was
	// holding. Go keeps an entire array alive while any slice references any part
	// of it, so without this the dropped values stay resident behind the returned
	// slice and every length-based assertion still passes. O(drop), and drop is 1
	// in the steady state.
	clear(xs[:drop])
	return xs[drop:]
}

// At resolves the newest version effective at or before asOf. ok=false when the
// key has no version yet, or when asOf predates every version retained — which
// after a restart, or beyond the horizon, is the same answer and is the honest
// one: the store does not know, and a caller that substitutes a zero for it is
// pricing off a number nobody computed.
func At[T any](vs []Version[T], asOf time.Time) (T, bool) {
	i := sort.Search(len(vs), func(i int) bool { return vs[i].AsOf.After(asOf) })
	if i == 0 {
		var zero T
		return zero, false
	}
	return vs[i-1].V, true
}

// Horizon returns d when it is positive and DefaultHorizon otherwise. The
// stores funnel their option through it so an unset horizon is the documented
// default everywhere rather than "retain forever" in whichever store forgot.
func Horizon(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultHorizon
	}
	return d
}

package alpha

import (
	"context"
	"errors"
	"time"
)

// THE SECOND SEAM (#416, owner ruling 2026-08-20).
//
// MarketView above is the edge's live, in-memory microstructure — the order book
// and the trade tape, as they are RIGHT NOW. BarView below is the other half an
// alpha engine needs and could not reach: the durable OHLCV series, read
// POINT-IN-TIME, with the C1 indicator set computed over it.
//
// # Why this is a second interface and not four more methods on MarketView
//
// The 2026-08-12 ruling in #416 moved MarketView to EXECUTION QUALITY — deciding
// when, inside a decision window, to place — and put ALPHA at intraday-to-
// multi-day over bars. Widening MarketView merges exactly what that ruling
// separated: an execution-quality consumer would inherit a bar dependency it has
// no use for, an alpha engine an order-book dependency it cannot use at its
// horizon, and the day either one needs to change, both move.
//
// The asymmetry of the two repairs is what settles it. Merging two narrow
// interfaces later is a refactor. Splitting one wide one that engines have
// already been written against is not.
//
// Nothing that consumes MarketView today changes.
//
// # POINT-IN-TIME IS THE PROPERTY THAT DECIDES THE SHAPE
//
// An alpha engine graded on history is worthless if it can see a bar that had
// not printed when it decided. A leak here does not fail loudly: the numbers stay
// in range, the backtest reads BETTER than the truth, and every model selected on
// top of it was selected for its ability to read the future.
//
// So a view is built for one decision time and REFUSES to reach past it —
// ErrLookahead, never a silently truncated window. The implementation
// (internal/alpha/barview) closes the three ways the leak actually happens:
//
//	the bar CONTAINING the decision time    had not completed; its close was
//	                                        not knowable. Excluded.
//	a bar starting AFTER it                 had not printed. Excluded.
//	a CORRECTION that arrived later         the store is bitemporal; the read is
//	                                        bounded on knowledge time too.
//
// # IT AUTHORISES NOTHING
//
// This seam lets an engine READ. What an engine may DO with a prediction is not
// decided here and is deliberately still open: #112 declined to construct the
// resilient inference client for exactly that reason, and #574 armed
// RISK_REQUIRE_VALIDATED_ANALYTICS so an unvalidated analytic refuses the
// engine's start. Nothing below returns an order, a size or a permission.

// ErrLookahead refuses a read that would require an observation later than the
// view's decision time.
//
// IT IS A REFUSAL, NOT A CLAMP. Clamping the request back to the decision time
// would answer a question the caller did not ask, with a window it has no reason
// to distrust — and a backtest whose reads were quietly re-aimed reports a
// strategy nobody ran. The one direction this must never fail in is answering.
var ErrLookahead = errors.New("alpha: look-ahead refused")

// Coverage is how much of the window a Reading was computed over can be spoken
// for. It is the projection of store.Attested across this seam.
//
// # IT IS NOT store.Attested, and the reason is a build-graph one
//
// internal/marketdata/store carries the Postgres implementation, so importing it
// here would put a database driver into every binary that links this package —
// and the binary that links it is market-ingest, the low-latency fold at the
// edge, which has no database and today does not depend on pgx at all. The gap
// ARITHMETIC is not duplicated: store.AttestedWindowOf computes it, once, in the
// implementation, and this carries the four counts it produced.
//
// # THE THREE STATES, AND WHY Unknown IS ITS OWN NUMBER
//
//	a bar exists                                → Observed
//	no bar, and coverage says we were looking   → QUIET. A measured absence.
//	no bar, and coverage says nothing           → UNKNOWN. Nobody can speak for it.
//
// "No coverage record for this window" and "covered, and the market was quiet"
// must never look the same. An engine sizing off a window wants to know HOW MUCH
// of it nobody can vouch for, not a boolean it inherited.
type Coverage struct {
	// Buckets is how many COMPLETE buckets the window contains.
	Buckets int
	// Observed is how many of them have a bar.
	Observed int
	// Quiet is how many missing buckets the ingestion-coverage record attests the
	// platform was watching for the WHOLE bucket: real, measured absences.
	Quiet int
	// Unknown is how many missing buckets have no whole-bucket attestation.
	Unknown int
}

// Missing is how many buckets in the window hold no bar.
func (c Coverage) Missing() int {
	if c.Observed >= c.Buckets {
		return 0
	}
	return c.Buckets - c.Observed
}

// Sound reports that every absence in the window is accounted for.
//
// READ IT AS "this window supports a claim ABOUT THE WHOLE WINDOW" — never as
// "the readings are trustworthy", which is the asymmetry below and the opposite
// mistake. A window of zero buckets is Sound vacuously; a caller gating on this
// must decide what an empty window means for it rather than inheriting an answer
// from here.
func (c Coverage) Sound() bool { return c.Unknown == 0 }

// Reading is one point-in-time answer: what the indicator set said about this
// instrument, on this venue, using only what was knowable at the moment asked.
//
// # A SIGNAL PROVABLE FROM A PARTIAL WINDOW IS STILL PROVABLE (#594)
//
// This is the asymmetry #594 established one package over, in
// internal/alpha/outcome, and it holds here for the same reason:
//
//	a reading over an UNBROKEN run of the length it claims  → provable. Holes
//	                                                          earlier in the
//	                                                          window cannot
//	                                                          un-observe it.
//	a claim about the WHOLE window ("nothing spiked in the
//	last 200 minutes")                                      → needs Sound().
//
// So Indicators is populated whenever it CAN be, and Coverage is reported beside
// it rather than gating it. Refusing every gapped window would throw away every
// provable signal — and internal/marketedge/bars/fold.go emits NO BAR for a
// minute in which nothing traded, so a gapped window is the NORMAL shape of this
// input, not an exotic one. An engine that needs the stronger claim consults
// Coverage.Sound(); one that does not, does not pay for it.
type Reading struct {
	// At is the market time this answers for. It is also the knowledge horizon
	// the read was bounded on — the two are the same here by construction, and a
	// view that bounded one without the other would leak invisibly.
	At time.Time

	// Indicators is the C1 set (internal/marketdata/indicator) over the
	// contiguous run ending at the last COMPLETED bar.
	//
	// A MISSING READING IS AN ABSENT KEY, NEVER A ZERO. 0 is a valid RSI meaning
	// "sold off hard" and a valid realized vol meaning "did not move"; either
	// written in place of "not enough unbroken history" is indistinguishable from
	// a real reading, which is the failure this estate refuses everywhere else.
	Indicators map[string]float64

	// Contiguous is how many bars, ending at the most recent completed one, form
	// the unbroken run the readings were computed over. It is the DURATION the
	// readings actually claim, in buckets — a period here is a duration, not an
	// element count, and over a gapped series those two silently diverge.
	Contiguous int

	// Close is the close of the last bar to COMPLETE at or before At, and CloseOK
	// says whether there was one.
	//
	// THE LAST COMPLETED BAR, not the bar containing At: that bar had not
	// finished, so its close was not knowable — and the leak is the flattering
	// kind, because an engine firing mid-bar on a move would be handed a price
	// that already contains the move it reacted to. internal/alpha/outcome makes
	// the identical choice for the reference price it grades against, and the two
	// must agree or a model is graded from a price it never saw.
	Close   float64
	CloseOK bool

	// Coverage is the window the readings were computed over, accounted for. See
	// the asymmetry above: it is reported, never a gate.
	Coverage Coverage
}

// BarView is the read-seam over the durable OHLCV series for ONE instrument on
// ONE venue, pinned to one decision time.
//
// # ONE VENUE, NEVER MERGED — the same rule MarketView keeps
//
// store.Bar makes venue part of a bar's IDENTITY: a model trained on a composite
// candle and executed against one venue's book shows a backtest/live divergence
// that reads as "the strategy stopped working" rather than as a data defect. A
// cross-venue engine takes one view per venue and compares them itself.
//
// # IMPLEMENTATIONS MUST BE IMMUTABLE
//
// An engine's Evaluate runs on the edge's tick loop, concurrently with the fold
// goroutines mutating the books beneath it. A view whose decision time could be
// advanced in place would be shared mutable state on exactly that path — so a
// view is built for one decision time and a live engine builds a fresh one per
// tick, which is a struct allocation and not a synchronisation problem.
type BarView interface {
	// InstrumentID is the canonical Kanz instrument.
	InstrumentID() string
	// MIC is the venue whose candles this reads (e.g. "XBIN", "OKEX").
	MIC() string
	// DecisionTime is the horizon this view refuses to read past.
	DecisionTime() time.Time

	// At returns the reading for market time `at`, using only what Kanz knew by
	// then.
	//
	// It returns ErrLookahead when `at` is after DecisionTime. Insufficient
	// history is NOT an error: it is a Reading with fewer keys, a Contiguous of
	// zero and a Coverage that says so.
	At(ctx context.Context, at time.Time) (Reading, error)
}

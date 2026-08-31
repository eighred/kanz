// Package marketview binds the market-data edge to the execution algorithms
// (#869).
//
// # What it is
//
// internal/execution/algo declares MarketView — "what an algorithm may know about
// the market it is working into" — and ships exactly one implementation of it,
// UnknownMarket, which answers UNKNOWN to everything. That was correct and it was
// also the whole story: nothing on this platform could answer a volume question,
// so nothing could work an order by volume.
//
// internal/marketedge/volprofile (#867) folds the datum that answers one:
// a per-(instrument, venue) intraday distribution of traded volume. This package
// is the translation between them, and it is a package rather than a method
// because neither side may depend on the other. The algo package is bound to
// purity by test/arch/schedule_is_derived_test.go — it may not import a store, it
// may not hold a mutex, and volprofile.Store is both — while volprofile is a fold
// on the market-data edge that must not know what a schedule is.
//
// # WHY THE AS-OF IS FIXED WHEN THE VIEW IS BUILT
//
// A view is constructed for ONE decision, with the instant that decision is being
// made, and answers every question as of it. That is what lets an algorithm be a
// pure function while still reading market data: the clock is read by the DRIVER
// building the view, never by the schedule, which is the division algo.MarketView
// states and the guard enforces. It is also what makes a backtest possible at all
// — volprofile.Profile refuses a zero as-of outright, and refuses one that
// precedes its newest completed session, because a read defaulting to now is
// total look-ahead when replayed over history.
//
// # WHAT AN UNKNOWN LOOKS LIKE FROM HERE, AND WHY THERE ARE SO MANY
//
// Every path that cannot produce a measured number answers `known=false`, and
// there is no path that answers a zero instead. A series nobody has folded, a
// history shorter than the desk requires, a shape older than the retention
// horizon, a read that would look ahead, an interval longer than the history the
// shape was built from — all of them are UNKNOWN, and the algorithm refuses the
// order rather than working it against a curve nobody measured.
//
// That is the direction #869 requires and the reason it named the profile's
// UNKNOWN as load-bearing: the alternative is a flat curve, a flat curve is TWAP,
// and a VWAP order worked as TWAP is a mislabelled execution whose fills the
// attribution plane will decompose against an algorithm that never ran.
package marketview

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/execution/algo"
	"github.com/eighred/kanz/internal/marketedge/volprofile"
)

// ErrConfig refuses a view that could never answer anything.
//
// LOUD AT WIRING TIME RATHER THAN UNKNOWN AT DECISION TIME. A nil store or an
// unnamed venue would answer UNKNOWN to every question, which is exactly what a
// correctly-wired view answers when a profile is genuinely absent — so an
// operator would read "no volume profile" and go looking at the market feed while
// the fault was in the composition root. "Nothing configured" and "checked, and
// nothing is known" must not look the same.
var ErrConfig = errors.New("marketview: configuration")

// Volume answers volume questions from an intraday volume profile.
//
// IT IS A VALUE FOR ONE DECISION. It holds no lock and mutates nothing; the store
// behind it is safe for concurrent use, so a driver may build one per parent
// order per tick and throw it away.
type Volume struct {
	store *volprofile.Store
	venue string
	asOf  time.Time
}

// NewVolume builds a view over one venue's profiles, answering as of an instant.
//
// The VENUE IS THE VIEW'S, NOT THE QUESTION'S, because volprofile keys a shape by
// (instrument, venue) and an order executes against ONE book. A view that guessed
// the venue per question, or blended venues, would schedule an order against a
// shape made of books it will never touch — the exact claim #867 refuses in its
// own doc.
func NewVolume(store *volprofile.Store, venue string, asOf time.Time) (*Volume, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: no volume profile store", ErrConfig)
	}
	if venue == "" {
		return nil, fmt.Errorf("%w: a profile is keyed by (instrument, venue) and this view names "+
			"no venue, so every question it answered would be about a book nobody chose", ErrConfig)
	}
	if asOf.IsZero() {
		return nil, fmt.Errorf("%w: as-of is required and does not default to now — a read that "+
			"defaults is correct for a live scheduler and total look-ahead for a backtest", ErrConfig)
	}
	return &Volume{store: store, venue: venue, asOf: asOf.UTC()}, nil
}

// AsOf is the instant this view answers as of.
func (v *Volume) AsOf() time.Time { return v.asOf }

// TopOfBook answers UNKNOWN, always.
//
// A VOLUME PROFILE HOLDS NO PRICES AND #867 REFUSES TO READ ONE — its Observe
// deliberately ignores a trade's price so nobody can mistake the fold for
// something that says where an instrument trades. So this is not a stub awaiting
// an implementation: it is the honest answer for this view. A price-aware view is
// a different binding over a different datum, and until one exists an algorithm
// that needs a book refuses here rather than being handed a number.
func (v *Volume) TopOfBook(string) (*big.Rat, *big.Rat, bool) { return nil, nil, false }

// ExpectedVolume is the quantity expected to trade in [from, to).
//
// # Shape times level
//
// A profile is a DISTRIBUTION — its shares sum to 1 — so on its own it answers
// "which part of the day is busy" and cannot answer "how many units". The answer
// here is the share of a session falling in [from, to) multiplied by
// Answer.SessionVolume, the mean total of the sessions the shape was built from.
// Both halves come from the same set of retained sessions, computed in the same
// pass, so the level cannot describe a different market from the shape.
//
// # The share is INTEGRATED, not looked up
//
// A slice's interval is whatever the parent's window divided by its slice count
// happens to be, and it will not line up with the profile's 30-minute bins. So the
// share is the cumulative distribution evaluated at both ends: whole sessions
// between them contribute 1 each, and each end contributes the bins it fully
// covers plus a PRO-RATA part of the bin it lands inside.
//
// THE PRO-RATA IS AN ASSUMPTION AND IT IS STATED: volume is taken as uniform
// WITHIN a bin, because a bin is the finest thing the profile measured and
// anything smaller is a shape nobody sampled. It is the same assumption a caller
// makes by choosing the bin width, applied consistently rather than by rounding an
// interval to the nearest bin — which would put two different slices of the same
// parent on the same share and size them identically.
func (v *Volume) ExpectedVolume(instrumentID string, from, to time.Time) (*big.Rat, bool) {
	if instrumentID == "" {
		return nil, false
	}
	from, to = from.UTC(), to.UTC()
	if to.Before(from) {
		// A backwards interval is not a question about this market. It is refused
		// as UNKNOWN rather than answered with a negative or an absolute value,
		// either of which would size a child from a caller's own bug.
		return nil, false
	}

	// A WINDOW LONGER THAN THE HISTORY THE SHAPE WAS BUILT FROM. The store's own
	// horizon is the bound, read from the store rather than chosen here: beyond it
	// the answer is an extrapolation of a mean over sessions nobody sampled, and
	// this view does not extrapolate.
	if horizon := v.store.Horizon(); horizon > 0 && to.Sub(from) > horizon {
		return nil, false
	}

	ans, err := v.store.Profile(volprofile.Series{InstrumentID: instrumentID, Venue: v.venue}, v.asOf)
	if err != nil {
		// ErrNoAsOf cannot occur (the constructor refuses a zero) and ErrLookahead
		// can: a driver whose as-of precedes the end of the newest completed
		// session is asking to be told about a session it could not have seen.
		// Both are UNKNOWN, which is what makes the algorithm refuse.
		return nil, false
	}
	if !ans.Known() || len(ans.Shares) == 0 || ans.Bucket <= 0 {
		return nil, false
	}
	if ans.SessionVolume == nil || ans.SessionVolume.Sign() <= 0 {
		// A KNOWN SHAPE WITH NO LEVEL IS STILL UNKNOWN FOR THIS QUESTION. The
		// shares would answer "which half of the window is busier" perfectly well,
		// and multiplying them by nothing would answer "how much" with a zero.
		return nil, false
	}

	share := shareOver(ans, from, to)
	if share.Sign() < 0 {
		return nil, false
	}
	return new(big.Rat).Mul(share, ans.SessionVolume), true
}

// shareOver is the fraction of a session's volume expected in [from, to), which
// exceeds 1 when the interval spans more than a session.
func shareOver(a volprofile.Answer, from, to time.Time) *big.Rat {
	// WHOLE SESSIONS BETWEEN THE TWO ENDS, counted from the truncated boundaries
	// rather than from the raw difference: [23:00 Monday, 01:00 Tuesday) spans two
	// hours and one session boundary, and only the boundary count is the number of
	// complete distributions between the prefixes below.
	sessions := to.Truncate(volprofile.Session).Sub(from.Truncate(volprofile.Session)) / volprofile.Session

	out := new(big.Rat).SetInt64(int64(sessions))
	out.Add(out, prefixShare(a, to))
	out.Sub(out, prefixShare(a, from))
	return out
}

// prefixShare is the share of a session accumulated from its start up to t.
func prefixShare(a volprofile.Answer, t time.Time) *big.Rat {
	off := t.Sub(t.Truncate(volprofile.Session))
	if off < 0 {
		return new(big.Rat)
	}

	full := int(off / a.Bucket)
	out := new(big.Rat)
	for i := 0; i < full && i < len(a.Shares); i++ {
		out.Add(out, a.Shares[i])
	}
	if full >= len(a.Shares) {
		return out
	}

	// THE PART-BIN, PRO-RATA. Exact rationals throughout: the remainder and the
	// bin are both integer nanosecond counts, so the fraction is exact and a
	// schedule derived from it is reproducible to the last digit on every pod.
	rem := off - time.Duration(full)*a.Bucket
	if rem <= 0 {
		return out
	}
	part := new(big.Rat).SetFrac64(int64(rem), int64(a.Bucket))
	return out.Add(out, part.Mul(part, a.Shares[full]))
}

// Volume is an algo.MarketView. The assertion is here rather than in a test
// because a drift in the interface must fail the BUILD: a view that no longer
// satisfies it would leave the algorithms with UnknownMarket as their only
// implementation, and every volume-driven order refused for a reason that names
// the market rather than the wiring.
var _ algo.MarketView = (*Volume)(nil)

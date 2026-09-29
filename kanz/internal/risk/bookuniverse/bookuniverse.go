// Package bookuniverse derives the factor-model estimation universe from the
// risk engine's live book — the production compute.UniverseFunc, and the last
// data seam NewLiveModelProvider was missing (#509).
//
// Until it existed the whole factor branch was unreachable: RegisterFactorRisk
// needs a ModelProvider, LiveModelProvider is the production one, and it could
// not be constructed. A client asking for a factor measure got a 200 with the
// measure absent, which is indistinguishable from a portfolio that has no factor
// exposure.
//
// # THE UNIVERSE IS THE BOOK, NOT THE MARKET
//
// Every instrument held in any portfolio, and nothing else. That is the right
// set because a loading is only ever consulted for an instrument the firm holds
// (factormodel.Model.Loading answers false outside the universe, surfaced as a
// coverage concern), while every instrument added widens the covariance being
// estimated from the same fixed return window. Fitting over a broader "market"
// list would degrade the estimate for the positions that actually carry risk.
//
// # DEDUPLICATED, BECAUSE Fit DOES NOT
//
// One instrument held in five portfolios must be ONE column. factormodel.Fit
// sorts its universe but never dedupes it: duplicate ids become duplicate rows
// in the return panel — perfectly correlated columns, a singular covariance for
// the PCA to eigendecompose — and both Model.index and Model.SpecificVar are
// keyed by id, so the duplicate rows silently collapse to whichever was written
// last. The model would be built and would be wrong, with nothing to say so.
//
// # SORTED HERE TOO, THOUGH Fit SORTS
//
// Fit sorting internally is what makes the fit deterministic; sorting here is
// what makes THIS function's output deterministic for every other consumer —
// a log line, a metric label, a cache key, a diff between two as-ofs. Leaving it
// to Fit would make this package's determinism a property of a different
// package's implementation detail, and Go's randomised map iteration means the
// unsorted result would differ run to run without ever being wrong enough to
// notice.
package bookuniverse

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/factormodel"
	"github.com/eighred/kanz/internal/risk/state"
)

// asOf IS ACCEPTED AND CANNOT BE HONOURED, AND THAT COSTS SOMETHING
//
// compute.UniverseFunc is a point-in-time seam: (ctx, asOf) → the universe as it
// stood then. state.Store cannot answer that question. It is the engine's live
// state-of-the-world — one Portfolio per id, folded forward from the bus, with
// no history behind it. So what this returns is WHAT THE BOOK HOLDS NOW,
// whatever asOf says.
//
// The cost is precise and it is not hypothetical: a recompute as of last year
// gets today's universe. An instrument bought last week appears in a model fit
// as of last month. The leak is confined to MEMBERSHIP — factormodel reads every
// return series through the point-in-time ReturnsProvider, so the numbers behind
// each column are honest — but a universe of today's holdings is a survivorship
// selection, and an EVT-21d replay of a historical breach therefore reproduces
// today's book rather than the one that existed. Treat a historical factor
// number as indicative, not as evidence.
//
// Closing it needs a point-in-time position source (the durable snapshot history
// the persistence layer already writes), not a change here. Until then the case
// is at least COUNTABLE rather than silent: WithLookaheadObserver fires whenever
// the caller asks about an instant older than the book's own state, which is the
// exact provable form of "you asked for history and got the present".

// DefaultMinInstruments is the smallest universe this will return rather than
// refuse. It is factormodel.DefaultStatFactors + 1, and the +1 is arithmetic
// rather than taste.
//
// fitStatistical keeps the top k principal components and clamps k to n
// ("if k > n { k = n }"). At n <= k the kept eigenvectors span the entire return
// space, so every instrument's explained variance equals its total variance and
// SpecificVar comes out zero across the whole book. That model reports NO
// idiosyncratic risk at all — a number portfolio VaR will consume without
// complaint — and it is an artifact of the universe size, not a finding about
// the book. k+1 is the smallest n at which the fit is not degenerate in that way.
//
// IT IS A FLOOR, NOT AN ADEQUACY THRESHOLD, and the difference matters. A
// universe that merely clears it still estimates a k-factor covariance from k+1
// series; a defensible statistical minimum scales with both the factor count and
// the return window, and this package is handed no factormodel.Config so it can
// see neither. A caller who raises Config.StatFactors above DefaultStatFactors
// MUST raise this to match via WithMinInstruments — nothing here can detect that
// they did not.
const DefaultMinInstruments = factormodel.DefaultStatFactors + 1

// AN EMPTY BOOK IS AN ERROR, NOT AN EMPTY UNIVERSE.
//
// At startup the store is empty and fills from the bus, so "no book loaded yet"
// is a real and expected state — which is exactly why it must not be returned as
// a successful empty slice. An empty slice with a nil error says "checked, and
// the firm holds nothing", and AGENTS.md's rule is that this must never look the
// same as "nothing has arrived".
//
// At today's only consumer the two are indistinguishable — LiveModelProvider.
// Model collapses `err != nil` and `len(instruments) == 0` into the same ok=false
// — so the error costs nothing there and is the only thing that carries a reason
// anywhere else. The load-bearing half is what it prevents: the next consumer of
// this seam, one that does not happen to guard on length, would hand the empty
// slice to factormodel.Fit and fit a model over nothing.
var (
	// ErrEmptyBook: no portfolio holds any instrument. The operator's next move
	// is upstream — ingestion has not filled the store.
	ErrEmptyBook = errors.New("bookuniverse: no instrument is held in any portfolio")
	// ErrUniverseTooSmall: the book is real and too small to estimate a factor
	// model over. The next move is the mandate, or a deliberate floor change.
	ErrUniverseTooSmall = errors.New("bookuniverse: universe below the minimum for a factor fit")
)

// Option customizes the universe function.
type Option func(*book)

// WithMinInstruments overrides DefaultMinInstruments. Raise it when the model
// config keeps more factors than factormodel.DefaultStatFactors; see the
// DefaultMinInstruments doc for why the floor is tied to that number.
func WithMinInstruments(n int) Option {
	return func(b *book) { b.min = n }
}

// WithLookaheadObserver sets the hook fired when the caller's asOf predates the
// newest state in the book — the provable form of "this universe is the present,
// and you asked about the past". Nil (the default) leaves the case unobserved,
// which is the state before this package existed; wire a counter at the
// composition root so a backtest cannot quietly acquire a survivorship universe.
func WithLookaheadObserver(fn func(asOf, newestHeld time.Time)) Option {
	return func(b *book) { b.onLookahead = fn }
}

// book holds the closure's configuration.
type book struct {
	store       *state.Store
	min         int
	onLookahead func(asOf, newestHeld time.Time)
}

// FromStore returns the compute.UniverseFunc over the engine's live state.
//
// It refuses a nil store and an unusable floor HERE rather than at call time,
// because both are provable at config time and neither survives the trip
// otherwise: LiveModelProvider.Model discards the error and reports ok=false, so
// a wiring mistake surfaced through the returned function would show up as
// factor measures reporting zero forever, with no signal naming the cause. The
// return type is also the compile-time assertion against compute.UniverseFunc —
// a signature drift is caught here, not at the composition root.
func FromStore(s *state.Store, opts ...Option) (compute.UniverseFunc, error) {
	if s == nil {
		return nil, fmt.Errorf("bookuniverse: a state store is required — the universe is the book, " +
			"and there is no other source for it")
	}
	b := &book{store: s, min: DefaultMinInstruments}
	for _, o := range opts {
		o(b)
	}
	if b.min < 1 {
		return nil, fmt.Errorf("bookuniverse: minimum universe size %d is not usable; 0 is not a way "+
			"to switch the floor off, because an empty universe is refused unconditionally (a fit "+
			"over nothing is not a model)", b.min)
	}
	return b.universe, nil
}

// universe implements compute.UniverseFunc: the deduplicated, sorted set of
// instrument ids across every portfolio in the store.
func (b *book) universe(ctx context.Context, asOf time.Time) ([]string, error) {
	// The read is entirely in memory, so there is nothing to interrupt part way
	// through and no per-portfolio check below; this only declines to start work
	// for a caller that has already given up.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ids := b.store.IDs()
	held := make(map[string]struct{}, len(ids))
	var newestHeld time.Time
	for _, id := range ids {
		// Snapshot, not Lookup: the clone is taken under the per-aggregate lock,
		// so the position set cannot tear against a concurrent apply. It costs one
		// map copy per portfolio per call, paid once per as-of because
		// LiveModelProvider caches the fit — not once per measure.
		p, ok := b.store.Snapshot(id)
		if !ok {
			// Raced with a removal between IDs() and Snapshot(). A portfolio that
			// no longer exists holds nothing.
			continue
		}
		if a := p.AsOf(); a.After(newestHeld) {
			newestHeld = a
		}
		for _, pos := range p.Positions() {
			// A FLAT POSITION STAYS IN THE UNIVERSE. domain retains zero-quantity
			// positions for audit lineage until a state event forgets them;
			// dropping them here would make the model universe disagree with
			// Portfolio.Positions(), so every instrument the book deliberately
			// still tracks would read as a model coverage gap — and a re-opened
			// position would sit outside the model until the next fit.
			held[string(pos.InstrumentID)] = struct{}{}
		}
	}

	if len(held) == 0 {
		// The portfolio count separates "the bus has delivered nothing" from
		// "portfolios exist and hold nothing", which have different causes and the
		// same empty universe.
		return nil, fmt.Errorf("%w (%d portfolio(s) in the store)", ErrEmptyBook, len(ids))
	}
	if len(held) < b.min {
		return nil, fmt.Errorf("%w: %d instrument(s) across %d portfolio(s), minimum %d",
			ErrUniverseTooSmall, len(held), len(ids), b.min)
	}

	out := make([]string, 0, len(held))
	for id := range held {
		out = append(out, id)
	}
	sort.Strings(out)

	if b.onLookahead != nil && asOf.Before(newestHeld) {
		b.onLookahead(asOf, newestHeld)
	}
	return out, nil
}

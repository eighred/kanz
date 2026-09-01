package marketview

import (
	"fmt"
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/execution/algo"
)

// Pinned answers volume questions from ONE published, versioned profile (#897).
//
// # Why the OMS cannot use Volume
//
// Volume resolves its answer from a live volprofile.Store, and that is correct
// where the fold is maintained and wrong everywhere else. The store is
// per-process and in-memory: it starts empty, answers ABSENT until MinSessions
// sessions accumulate, and MOVES every time a session completes. Three
// consequences, and the third is the one that breaks the OMS.
//
// A pod that rolled refuses every volume-driven order for as many days as the
// desk's MinSessions demands, because its own fold has not caught up.
//
// A schedule derived at 23:59 and re-derived at 00:01 is derived against two
// different curves, because a session closed in between.
//
// And two pods hold two folds. services/oms/internal/order.authorizeChild
// re-derives a parent's schedule to decide whether an inbound child really is a
// slice of it, and it compares the quantity as an EXACT RATIONAL. Two curves
// differing in the last digit refuse the driver's own child as a forgery, and the
// parent stops advancing while every screen shows it working.
//
// # What a pin buys, stated as the property rather than as the mechanism
//
// SAME INPUT, SAME SCHEDULE. The parent order records which profile VERSION its
// schedule was planned against, and every later derivation — the child-admission
// check on one pod, the driver tick on another, the same pod after a restart —
// resolves that version and holds it here. The curve is then an input the order
// stores durably, exactly like its quantity and its window, which is what
// test/arch/schedule_is_derived_test.go's whole argument rests on.
//
// It is also what makes a backtest possible. A pinned shape is a value, not a
// read: replaying a parent order reaches the curve it was actually sized against
// rather than today's, which is the look-ahead volprofile.Profile refuses in the
// live direction and this refuses in the historical one.
//
// # It answers about ONE instrument and says so
//
// A Volume serves every instrument its store folds. A Pinned holds one resolved
// curve, so a question about any other instrument is UNKNOWN rather than answered
// from a shape measured on something else. That is not a limitation of the pin —
// it is the pin: a version addresses one (instrument, venue) series, and a view
// that answered beyond it would be answering from a curve the order never named.
type Pinned struct {
	shape   Shape
	version string
}

// NewPinned builds a view over one resolved, published shape.
//
// The version is carried for the refusals and the logs rather than for the
// arithmetic: an operator reading "expected volume is UNKNOWN" needs to know
// WHICH pinned curve could not answer, or they cannot tell a profile that was
// evicted from a window that runs past the end of one.
func NewPinned(sh Shape, version string) (*Pinned, error) {
	if err := sh.validate(); err != nil {
		return nil, err
	}
	if version == "" {
		return nil, fmt.Errorf("%w: a pinned view names no version, so nothing could say WHICH "+
			"curve a schedule was derived against and the pin would be unauditable", ErrShape)
	}
	return &Pinned{shape: sh, version: version}, nil
}

// Version is the published profile this view answers from.
func (p *Pinned) Version() string { return p.version }

// InstrumentID is the one instrument this view can answer about.
func (p *Pinned) InstrumentID() string { return p.shape.InstrumentID }

// TopOfBook answers UNKNOWN, always.
//
// A VOLUME PROFILE HOLDS NO PRICES AND #867 REFUSES TO READ ONE — its Observe
// deliberately ignores a trade's price so nobody can mistake the fold for
// something that says where an instrument trades. So this is not a stub awaiting
// an implementation: it is the honest answer for this view, and an algorithm that
// needs a book refuses on it rather than being handed a number.
func (p *Pinned) TopOfBook(string) (*big.Rat, *big.Rat, bool) { return nil, nil, false }

// ExpectedVolume is the quantity expected to trade in [from, to).
//
// A QUESTION ABOUT ANOTHER INSTRUMENT IS UNKNOWN, NOT AN ANSWER FROM THIS CURVE.
// The pin addresses one series; answering beyond it would size a child from a
// shape measured on a different market, which is the failure the pin exists to
// make impossible, arrived at from the other end.
func (p *Pinned) ExpectedVolume(instrumentID string, from, to time.Time) (*big.Rat, bool) {
	if instrumentID == "" || instrumentID != p.shape.InstrumentID {
		return nil, false
	}
	return expectedOver(p.shape, from, to)
}

// Pinned is an algo.MarketView, asserted at BUILD time for the reason Volume's
// own assertion gives.
var _ algo.MarketView = (*Pinned)(nil)

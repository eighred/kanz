package algo

import (
	"errors"
	"fmt"
	"math/big"
	"time"
)

// The seam a second execution algorithm lands behind (#868).
//
// # What was here before
//
// TWAP was a free function and services/oms/internal/schedule called it by name.
// That made "which algorithm works this order" a property of the BUILD rather
// than of the order: adding VWAP meant editing the scheduler, and no order could
// say how it wanted to be worked even though order.v1.ExecutionSchedule has
// carried an `algo` field since #435. An institutional desk chooses per order —
// the same parent is worked differently on a quiet Tuesday and into a close — so
// hard-wiring one algorithm made a trading decision into a deployment decision.
//
// # What the seam is, and what it deliberately is not
//
// Algo is an interface over the EXISTING Plan and Slice types. Nothing about how
// a schedule is derived changed: it is still a pure function of fields the parent
// order already stores durably, still recomputed identically on every pod, still
// bound to that by test/arch/schedule_is_derived_test.go. The interface adds
// SELECTION, not state.
//
// It is not a plugin system. Registered() is a function returning a fresh slice,
// not a package-level map an init() writes into. There is no registration at
// runtime, no discovery, and no way for a caller to add an algorithm — because a
// map is mutable state, an init() ordering is a thing that can go wrong on one
// pod and not another, and this platform executes deterministically or not at
// all. The set of algorithms this build can work an order with is a property of
// the source, readable in one place, and enforced by
// test/arch/every_algo_is_reachable_test.go.
//
// # An unnamed or unknown algorithm is REFUSED
//
// Lookup never falls back. An order naming an algorithm this build does not
// implement is refused at admission under its own code; a schedule that names
// nothing is refused the same way. Defaulting to TWAP would put a schedule
// nobody chose in front of a venue and would label the resulting fills with an
// algorithm that never ran — which is exactly the attribution #866 is adding.

// Name is an execution algorithm's canonical name.
//
// IT IS THE WIRE ENUM'S NAME WITHOUT ITS PREFIX, deliberately: order.v1's
// EXECUTION_ALGO_TWAP maps to "TWAP" by construction rather than by a switch
// somebody has to remember to extend (see the OMS's algoNameOf). One vocabulary,
// one list, and an enum value nothing implements resolves to a name Lookup
// refuses rather than to a silent default.
type Name string

// NameTWAP is time-weighted average price — equal quantity at equal intervals,
// indifferent to volume and to price.
//
// The volume-driven names live beside their implementations: NameVWAP in vwap.go,
// NamePOV in pov.go (#869). One name, one file, one algorithm — so a name cannot
// be added here and left with nothing behind it.
const NameTWAP Name = "TWAP"

// ErrUnknownAlgo refuses a schedule naming an algorithm this build cannot work.
//
// A DISTINCT SENTINEL BECAUSE IT IS A DISTINCT REFUSAL. "This window does not
// move forward" is the operator's own arithmetic coming back at them; "this
// build does not implement VWAP" is a capability answer, and on a rolling deploy
// it is the difference between a bad order and a fleet where half the pods can
// work an order and half cannot. The caller maps it to its own refusal code.
var ErrUnknownAlgo = errors.New("algo: no such execution algorithm")

// MarketView is what an algorithm may know about the market it is working into.
//
// # UNKNOWN is a third value, and it is the whole point of this shape
//
// Every method returns a `known bool` beside its value, and a false there does
// NOT mean zero. A volume profile (#867) can be absent — never built for this
// instrument — or stale, and both must reach an algorithm as UNKNOWN so it can
// REFUSE rather than degrade into working a large order against a volume curve
// it invented. Returning 0.0 for "I could not tell you" is the failure this
// platform names first: unknown ⇒ 0.0 ⇒ proceed as if known.
//
// # Staleness is the VIEW's judgement, not the algorithm's
//
// An implementation that considers its data too old must answer known=false. It
// cannot be the algorithm's call, because an algorithm may not read a clock —
// a schedule that reads time.Now() computes a different answer on the pod that
// recovers a parent order than on the pod that lost it, which is the state this
// package exists to have none of, and test/arch/schedule_is_derived_test.go
// fails the build on it. The view runs in the driver, where a clock is allowed.
//
// TWAP asks this nothing, by design. It is indifferent to volume and to price,
// which is why it is the algorithm whose output can be asserted closed-form. VWAP
// and POV (#869) ask ExpectedVolume for every slice's own interval and REFUSE the
// whole plan on the first `known=false` — which is what this shape was added for.
type MarketView interface {
	// TopOfBook is the best bid and offer for an instrument. known=false ⇒ this
	// view cannot say — there is no book, or the one it has is not current.
	TopOfBook(instrumentID string) (bid, ask *big.Rat, known bool)

	// ExpectedVolume is the quantity expected to trade in [from, to).
	// known=false ⇒ UNKNOWN, which is what an absent or stale volume profile
	// must answer rather than a zero that reads as "nothing will trade".
	ExpectedVolume(instrumentID string, from, to time.Time) (qty *big.Rat, known bool)
}

// UnknownMarket answers UNKNOWN to everything.
//
// IT IS A VALUE A CALLER MUST PASS ON PURPOSE, which is the difference between
// "nothing configured" and "checked, and nothing is known". The OMS has no
// market data on the schedule path today and says so with this rather than with
// a nil that would panic or, worse, with a zero-valued view that would read as
// a market where nothing trades and no price exists.
type UnknownMarket struct{}

// TopOfBook answers UNKNOWN.
func (UnknownMarket) TopOfBook(string) (*big.Rat, *big.Rat, bool) { return nil, nil, false }

// ExpectedVolume answers UNKNOWN.
func (UnknownMarket) ExpectedVolume(string, time.Time, time.Time) (*big.Rat, bool) {
	return nil, false
}

// ParentState is what the parent has already DONE, as far as the caller can
// establish it.
//
// NOT A COUNT, AND NOT A CURSOR. Sent is a predicate over slice indices for the
// reason services/oms/internal/schedule gives at length: a count assumes children
// were created in order and that none was lost, and a crash between two
// creations leaves a hole a count would skip past forever.
type ParentState struct {
	// OrderID is the parent's own id, for the refusals an algorithm writes.
	OrderID string

	// Sent reports whether slice index has already been created as a child order.
	//
	// NIL MEANS UNKNOWN — the caller could not establish what has been sent. An
	// algorithm that needs to know must refuse rather than read nil as "nothing
	// was sent", which would re-derive a schedule that ignores everything already
	// in front of a venue. TWAP does not ask, so nil is correct for a caller that
	// works TWAP orders and has no cheaper answer.
	Sent func(index int) bool
}

// Algo turns a parent order into the child orders that work it.
//
// Schedule returns the WHOLE schedule, not the part that is due — filtering by
// the clock is Due's job and the driver's decision. An implementation must be a
// pure function of its three arguments: same inputs, same slices, on every pod
// and after every restart.
type Algo interface {
	// Name is the algorithm's canonical name, and it must be the one Registered
	// publishes it under.
	Name() Name

	// Schedule derives the children. It must refuse rather than degrade when the
	// market view cannot answer something it needs.
	Schedule(p Plan, st ParentState, mkt MarketView) ([]Slice, error)
}

// Registered is every algorithm this build can work an order with.
//
// A FUNCTION RETURNING A FRESH SLICE, NOT A PACKAGE-LEVEL MAP. Package-level
// mutable state in this package is what test/arch/schedule_is_derived_test.go
// exists to refuse, and the reason applies to a registry exactly as it applies
// to a schedule: anything that can differ between two calls can differ between
// two pods, and a fleet where one pod knows an algorithm and another does not is
// a parent order worked two ways.
//
// It is also the ONE list. Lookup reads it rather than repeating it, and
// test/arch/every_algo_is_reachable_test.go derives the implementation set from
// the source — every type in this package whose methods satisfy Algo — and fails
// if one of them is not constructed here. So an algorithm cannot be written and
// left unreachable, which is the failure that would otherwise be invisible: it
// compiles, its own tests pass, and no order can ever name it.
func Registered() []Algo {
	return []Algo{
		twapAlgo{},
		vwapAlgo{},
		povAlgo{},
	}
}

// Names is every registered algorithm's name, in Registered's order. Used by the
// refusals, so an operator whose order was refused learns what this build DOES
// implement rather than only that their choice failed.
func Names() []Name {
	all := Registered()
	out := make([]Name, 0, len(all))
	for _, a := range all {
		out = append(out, a.Name())
	}
	return out
}

// Lookup resolves a name to the algorithm that works it.
//
// IT NEVER FALLS BACK. An unset name and an unimplemented one are the same
// refusal — "work this order somehow" is not an instruction anybody can be held
// to, and picking one silently would put a schedule nobody chose in front of a
// venue and attribute the fills to an algorithm that never ran.
func Lookup(n Name) (Algo, error) {
	for _, a := range Registered() {
		if a.Name() == n {
			return a, nil
		}
	}
	if n == "" {
		return nil, fmt.Errorf("%w: the schedule names no algorithm; this build implements %v and "+
			"will not choose for you", ErrUnknownAlgo, Names())
	}
	return nil, fmt.Errorf("%w: %q; this build implements %v", ErrUnknownAlgo, string(n), Names())
}

// Run derives a parent's whole schedule under the algorithm its plan names.
//
// THE ONE ENTRY EVERY CALLER USES, so the name is resolved in exactly one place
// and no caller can reach an implementation without going through the registry.
// A nil market view is normalised to UnknownMarket rather than dereferenced:
// absence and "I know nothing" are the same answer to an algorithm, and the
// fail-closed direction — an algorithm that needs data refuses — is preserved
// either way. What is NOT normalised is the name, which must be present.
func Run(p Plan, st ParentState, mkt MarketView) ([]Slice, error) {
	a, err := Lookup(p.Algo)
	if err != nil {
		return nil, err
	}
	if mkt == nil {
		mkt = UnknownMarket{}
	}
	return a.Schedule(p, st, mkt)
}

// twapAlgo is TWAP's registration. It holds nothing and decides nothing: the
// arithmetic is TWAP, unchanged, and this type is only how the registry reaches
// it.
//
// STATE AND MARKET ARE IGNORED, AND THAT IS THE ALGORITHM RATHER THAN A STUB.
// TWAP is equal quantity at equal intervals, indifferent to volume and to price;
// an implementation that consulted the book would not be TWAP. It is the reason
// TWAP's output can be asserted as an exact value while every other algorithm's
// is a distribution.
type twapAlgo struct{}

// Name is NameTWAP.
func (twapAlgo) Name() Name { return NameTWAP }

// Schedule is TWAP, delegated whole. There is no branch here, which is what
// makes "behaviour unchanged by the seam" provable rather than asserted — see
// TestAlgoRegistry_TWAPThroughTheSeamIsTheSameSchedule.
func (twapAlgo) Schedule(p Plan, _ ParentState, _ MarketView) ([]Slice, error) {
	return TWAP(p)
}

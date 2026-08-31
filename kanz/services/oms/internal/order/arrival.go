package order

import (
	"math/big"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
)

// ArrivalMarks is the reference-mark source the arrival stamp reads (#436).
//
// An interface rather than the concrete marketdata mark.Source so this package
// does not take a dependency on the price spine to admit an order, and so a test
// can hand it a mark without a broker.
type ArrivalMarks interface {
	// Mark is the SAFE accessor: it returns nil for an instrument that is
	// unknown or whose mark has aged out.
	Mark(instrument string) *big.Rat
	// Lookup is the DIAGNOSTIC accessor, used here only for the observation
	// time of a mark Mark has already vouched for.
	Lookup(instrument string) (price *big.Rat, asOf time.Time, seen bool)
	// Touch is the QUOTED WIDTH, and it is the safe accessor for it: ok is false
	// when the instrument was never quoted, when the newest quote has aged out,
	// or when the two legs are crossed (#866).
	//
	// IT IS A SEPARATE ACCESSOR FROM Mark BECAUSE THEY ANSWER SEPARATE
	// QUESTIONS, and a mark can exist without a touch — a last-trade print
	// carries a price and no width at all. Folding them into one call would
	// force an instrument with a live mark and no quote to be treated as having
	// neither, and would take the benchmark away from every trade-only feed on
	// the platform.
	Touch(instrument string) (bid, ask *big.Rat, asOf time.Time, ok bool)
}

// WithArrivalMarks supplies the reference-mark source used to stamp an order's
// arrival price at admission. Absent, orders carry no arrival mark and are
// simply not measurable — which is the honest state, not a silent zero.
func WithArrivalMarks(m ArrivalMarks) ServiceOption {
	return func(s *Service) { s.arrivalMarks = m }
}

// observation is ONE read of the price spine, taken at one instant and used for
// every field this file stamps.
//
// IT IS ONE READ AND NOT THREE, deliberately. Mark, Lookup and Touch each take
// the source's lock separately, so three calls spread across an admission could
// straddle a fold and produce a mid from one tick, an observation time from
// another and a width from a third — a market that never existed, written down
// as if it had. Taking the three accessors once and carrying the result is what
// makes arrival_price and release_price EQUAL BY CONSTRUCTION for an unsliced
// order, which is the property the timing leg depends on: a decision worked
// immediately must show zero drift, not a few basis points of jitter between two
// reads of the same cache.
type observation struct {
	mark *big.Rat
	asOf time.Time
	bid  *big.Rat
	ask  *big.Rat
}

// observe takes the single read. ok is false when there is no usable mark, which
// is the one condition that makes an order unmeasurable outright.
func (s *Service) observe(instrument string) (observation, bool) {
	if s.arrivalMarks == nil || instrument == "" {
		return observation{}, false
	}
	// IT READS Mark, NOT Lookup, FOR THE VALUE. Lookup returns an entry
	// regardless of expiry — its own doc says nothing on a decision path may call
	// it, "or the expiry bound is one `if` away from being lost". A stale mark is
	// not an arrival price: it is a price from before whatever moved the market,
	// and stamping it would bake a fictional benchmark into the record
	// permanently. Lookup is used only for the observation time of a mark Mark
	// has already accepted.
	price := s.arrivalMarks.Mark(instrument)
	if price == nil {
		return observation{}, false
	}
	o := observation{mark: price}
	if _, asOf, seen := s.arrivalMarks.Lookup(instrument); seen && !asOf.IsZero() {
		o.asOf = asOf.UTC()
	}
	if bid, ask, touchAt, ok := s.arrivalMarks.Touch(instrument); ok {
		o.bid, o.ask = bid, ask
		// THE STAMPED TIME IS THE STALEST COMPONENT'S, NOT THE FRESHEST.
		//
		// A mark and a touch are two observations with two times: the mark can be
		// a trade print from a second ago while the quote behind the width is ten
		// seconds old. Recording the newer of the two would describe this
		// observation as fresher than its worst part, and a consumer discounting
		// a cost figure for staleness would discount it too little — always in
		// the flattering direction, which is the failure mode this whole
		// measurement exists to avoid. The older time is the only one true of
		// EVERY field stamped from this read.
		if !touchAt.IsZero() && (o.asOf.IsZero() || touchAt.UTC().Before(o.asOf)) {
			o.asOf = touchAt.UTC()
		}
	}
	return o, true
}

// stampArrival records the market's price for this instrument at admission.
//
// UNSET WHEN UNKNOWN, NEVER ZERO. "We could not see the market" is a real and
// common state — a thinly-quoted instrument, a cold start before the price spine
// has delivered, a mark aged out past OMS_PRICE_MAX_AGE. A zero arrival price
// makes implementation shortfall infinite or nonsensical depending which way it
// is divided; worse, it would score those orders as zero-cost and drag every
// venue average toward whichever venue happens to trade the thinnest names.
// Downstream must skip an unmeasurable order, and it can only do that if the
// absence is visible.
//
// THE OBSERVATION TIME IS CARRIED SEPARATELY from accepted_at because the GAP
// between them is itself a cost input: an order benchmarked against a mark from
// thirty seconds ago is being measured against a market that has already moved,
// and a consumer that cannot see the staleness cannot discount the number.
func (s *Service) stampArrival(st *orderpb.OrderState, o observation) {
	if st == nil {
		return
	}
	d, ok := dec.ToProtoScaled(o.mark)
	if !ok {
		// Unrepresentable as an exact Decimal. Recording nothing is right: the
		// alternative is a rounded benchmark that reads as exact.
		return
	}
	st.ArrivalPrice = d
	if !o.asOf.IsZero() {
		st.ArrivalAt = timestamppb.New(o.asOf)
	}
}

// stampRelease records the market as it stood when THIS order was released to be
// worked — its own admission — including the width that was quoted (#866).
//
// # It is stamped on every order, and on a CHILD it is the point of the exercise
//
// arrival_price is inherited by a child, because the decision was the parent's.
// The market was not. The difference between the parent's arrival mark and each
// child's own release mark is the market's drift while the parent was being
// worked, and it is the TIMING leg of the implementation shortfall — the thing
// that separates "the algorithm traded badly" from "the market moved away while
// the algorithm waited". Without it a scheduled parent's cost is one number and
// a desk cannot tell which of two entirely different repairs it needs.
//
// # THE WIDTH IS UNSET WHEN THE MARKET WAS NOT QUOTED, AND NEVER ZERO
//
// A last-trade mark has no width. Stamping bid == ask == mark would report that
// crossing this market was free, which is the single most flattering lie
// available to an execution report: it pushes the entire cost of crossing into
// the impact residual, the one leg nobody can independently check. An unset pair
// makes the decomposition refuse for that decision instead, which is the honest
// answer and the one a coverage metric can act on.
func (s *Service) stampRelease(st *orderpb.OrderState, o observation) {
	if st == nil {
		return
	}
	d, ok := dec.ToProtoScaled(o.mark)
	if !ok {
		return
	}
	st.ReleasePrice = d
	if !o.asOf.IsZero() {
		st.ReleaseAt = timestamppb.New(o.asOf)
	}
	if o.bid == nil || o.ask == nil {
		return
	}
	// BOTH LEGS OR NEITHER. A half-stamped touch is a width nobody can compute,
	// and a consumer holding one leg would have to guess whether the other was
	// absent or zero.
	bid, bidOK := dec.ToProtoScaled(o.bid)
	ask, askOK := dec.ToProtoScaled(o.ask)
	if !bidOK || !askOK {
		return
	}
	st.ReleaseBid, st.ReleaseAsk = bid, ask
}

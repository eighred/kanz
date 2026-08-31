package order

import (
	"math/big"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

// THE ARRIVAL MARK IS STAMPED AT ADMISSION, OR NOT AT ALL (#436).
//
// Transaction-cost analysis compares what an order paid against what the market
// showed when the decision was made. That benchmark cannot be recovered later: a
// price read at analysis time is a price from AFTER the order moved the market,
// which biases every cost measure flatter and always in the flattering
// direction. A cost number that understates cost is worse than no number,
// because it is the only number on the page and it will be believed.
//
// Nothing reads this field yet. It is stamped now because every order admitted
// without it is permanently unmeasurable — the same retroactive damage #432
// fixed one field over, where OKX bars recorded a busy minute as a dead market
// and the loss only became visible years later in the training set.

// fakeMarks is a mark source with a fixed answer.
type fakeMarks struct {
	price   *big.Rat
	asOf    time.Time
	seen    bool
	expired bool // Mark refuses an expired entry; Lookup still reports it

	// bid, ask and touchAt are the QUOTED WIDTH (#866). Zero means the
	// instrument was last seen as a trade print and has no observable width —
	// which is a different state from having a zero-width market, and the
	// distinction is what the release stamp exists to preserve.
	bid, ask *big.Rat
	touchAt  time.Time
}

func (f fakeMarks) Mark(string) *big.Rat {
	if f.price == nil || f.expired {
		return nil
	}
	return new(big.Rat).Set(f.price)
}

func (f fakeMarks) Lookup(string) (*big.Rat, time.Time, bool) {
	return f.price, f.asOf, f.seen
}

// Touch mirrors mark.Source.Touch: it is the SAFE accessor and refuses an
// expired or crossed quote rather than handing back a width nobody can use.
func (f fakeMarks) Touch(string) (*big.Rat, *big.Rat, time.Time, bool) {
	if f.bid == nil || f.ask == nil || f.expired || f.ask.Cmp(f.bid) <= 0 {
		return nil, nil, time.Time{}, false
	}
	return new(big.Rat).Set(f.bid), new(big.Rat).Set(f.ask), f.touchAt, true
}

var markObserved = time.Date(2026, 8, 1, 11, 59, 30, 0, time.UTC)

func stampWith(t *testing.T, m ArrivalMarks) *orderpb.OrderState {
	t.Helper()
	s := &Service{arrivalMarks: m}
	st := &orderpb.OrderState{InstrumentId: "BTC-USD"}
	// THE SAME SHAPE ADMISSION USES: one observation of the spine, both stamps
	// taken from it. Calling the two stampers with two separate reads would test
	// a code path the service does not have — and would hide the property the
	// timing leg depends on, that an unsliced order's arrival and release are
	// equal by construction.
	if o, ok := s.observe(st.GetInstrumentId()); ok {
		s.stampArrival(st, o)
		s.stampRelease(st, o)
	}
	return st
}

// A LIVE MARK IS RECORDED, with its own observation time.
func TestStampArrival_RecordsTheMarkAndItsObservationTime(t *testing.T) {
	st := stampWith(t, fakeMarks{price: big.NewRat(64000, 1), asOf: markObserved, seen: true})

	if st.GetArrivalPrice() == nil {
		t.Fatal("no arrival price stamped — the order is permanently unmeasurable")
	}
	if got := dec.FromProto(st.GetArrivalPrice()).RatString(); got != "64000" {
		t.Errorf("arrival_price = %s, want 64000", got)
	}
	// THE OBSERVATION TIME IS A SEPARATE FACT from admission time. The gap
	// between them is itself a cost input: an order benchmarked against a mark
	// from thirty seconds ago is measured against a market that already moved,
	// and a consumer that cannot see the staleness cannot discount the number.
	if st.GetArrivalAt() == nil {
		t.Fatal("no arrival_at stamped — the benchmark's age is unknowable")
	}
	if got := st.GetArrivalAt().AsTime(); !got.Equal(markObserved) {
		t.Errorf("arrival_at = %s, want %s", got, markObserved)
	}
}

// NO MARK ⇒ NOTHING STAMPED. Unknown is not zero.
//
// "We could not see the market" is a real and common state: a thinly-quoted
// instrument, a cold start before the price spine has delivered, a mark aged out.
// A zero arrival price makes implementation shortfall infinite or nonsensical
// depending which way it is divided — and worse, it would score those orders as
// ZERO COST, dragging every venue average toward whichever venue happens to
// trade the thinnest names.
func TestStampArrival_UnknownIsUnsetNotZero(t *testing.T) {
	st := stampWith(t, fakeMarks{}) // never seen

	if st.GetArrivalPrice() != nil {
		t.Fatalf("arrival_price = %v for an instrument with no mark, want UNSET. A zero "+
			"benchmark scores an unmeasurable order as zero-cost", dec.FromProto(st.GetArrivalPrice()))
	}
	if st.GetArrivalAt() != nil {
		t.Error("arrival_at stamped with no price behind it")
	}
}

// AN EXPIRED MARK IS NOT AN ARRIVAL PRICE, and this is the case the two
// accessors exist to separate.
//
// Lookup returns an entry REGARDLESS of expiry — its own doc says nothing on a
// decision path may call it, "or the expiry bound is one `if` away from being
// lost". A stale mark is a price from before whatever moved the market; stamping
// it would bake a fictional benchmark into the record permanently, and unlike a
// missing one it would look perfectly usable.
func TestStampArrival_AStaleMarkIsRefusedEvenThoughLookupStillReportsIt(t *testing.T) {
	m := fakeMarks{price: big.NewRat(64000, 1), asOf: markObserved, seen: true, expired: true}

	// Precondition: the diagnostic accessor still sees it, which is exactly the
	// trap — reading Lookup for the value would have stamped it.
	if _, _, seen := m.Lookup("BTC-USD"); !seen {
		t.Fatal("fixture is wrong: Lookup must still report an expired entry")
	}

	st := stampWith(t, m)
	if st.GetArrivalPrice() != nil {
		t.Fatalf("an EXPIRED mark was stamped as the arrival price (%v). It is a price from "+
			"before whatever moved the market, and it looks usable — which is worse than absent",
			dec.FromProto(st.GetArrivalPrice()))
	}
}

// NO MARK SOURCE CONFIGURED IS NOT A CRASH. A deployment with no price spine
// admits orders that are simply not measurable; refusing to admit them would
// turn a missing analytic into a trading outage.
func TestStampArrival_NoSourceIsHarmless(t *testing.T) {
	s := &Service{} // arrivalMarks nil
	st := &orderpb.OrderState{InstrumentId: "BTC-USD"}
	if _, ok := s.observe(st.GetInstrumentId()); ok {
		t.Fatal("observe reported a usable observation with no mark source configured")
	}
	if st.GetArrivalPrice() != nil {
		t.Error("an arrival price appeared with no mark source configured")
	}
}

// ADMISSION IS THE ONLY PLACE IT IS SET. Accept is documented pure — no I/O,
// identical on the live path and on replay — so a replay must not re-stamp an
// order with today's price. This pins that Accept itself leaves it alone.
func TestAccept_DoesNotStampAnArrivalPrice(t *testing.T) {
	st, err := Accept(&orderpb.SubmitOrder{
		OrderId:      "o-1",
		PortfolioId:  "fund-alpha",
		InstrumentId: "BTC-USD",
		Side:         orderpb.Side_SIDE_BUY,
		Quantity:     &commonpb.Decimal{Coefficient: 1},
		OrderType:    orderpb.OrderType_ORDER_TYPE_MARKET,
		TimeInForce:  orderpb.TimeInForce_TIME_IN_FORCE_IOC,
	}, time.Now())
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if st.GetArrivalPrice() != nil {
		t.Error("Accept stamped an arrival price — it is documented pure, and a replay would " +
			"then re-benchmark the order against today's market")
	}
}

// THE RELEASE OBSERVATION AND ITS WIDTH (#866).
//
// arrival_price alone is one observation, and a decomposition is a difference
// between two. These pin the second one: the market as it stood when THIS order
// was released to be worked, including the width that was quoted — without which
// the cost of crossing cannot be told apart from the cost of moving the book.

var touchObserved = time.Date(2026, 8, 1, 11, 59, 20, 0, time.UTC)

// A QUOTED MARKET IS RECORDED IN FULL: the mark, and both legs of the touch.
func TestStampRelease_RecordsTheQuotedWidth(t *testing.T) {
	st := stampWith(t, fakeMarks{
		price: big.NewRat(64000, 1), asOf: markObserved, seen: true,
		bid: big.NewRat(63990, 1), ask: big.NewRat(64010, 1), touchAt: markObserved,
	})

	if st.GetReleasePrice() == nil {
		t.Fatal("no release price stamped — the timing leg is a difference between the arrival " +
			"mark and this one, and without it a worked order's drift cannot be separated from " +
			"what its algorithm paid")
	}
	if got := dec.FromProto(st.GetReleasePrice()).RatString(); got != "64000" {
		t.Errorf("release_price = %s, want 64000", got)
	}
	if st.GetReleaseBid() == nil || st.GetReleaseAsk() == nil {
		t.Fatal("the quoted width was not stamped — the spread leg cannot be computed from a mid " +
			"at any later time, by anybody")
	}
	if got := dec.FromProto(st.GetReleaseBid()).RatString(); got != "63990" {
		t.Errorf("release_bid = %s, want 63990", got)
	}
	if got := dec.FromProto(st.GetReleaseAsk()).RatString(); got != "64010" {
		t.Errorf("release_ask = %s, want 64010", got)
	}
}

// AN UNSLICED ORDER'S ARRIVAL AND RELEASE ARE EQUAL BY CONSTRUCTION, and this is
// the property the timing leg depends on.
//
// The decision and the release are the same instant for an order that is not
// worked on a schedule, so its drift must be exactly zero — not a few basis
// points of jitter because the two stamps happened to read the mark cache twice
// and a tick landed in between. observe() takes ONE read for exactly this
// reason, and this pins it.
func TestStampRelease_AnUnslicedOrdersArrivalAndReleaseAreOneObservation(t *testing.T) {
	st := stampWith(t, fakeMarks{
		price: big.NewRat(64000, 1), asOf: markObserved, seen: true,
		bid: big.NewRat(63990, 1), ask: big.NewRat(64010, 1), touchAt: markObserved,
	})

	if dec.Cmp(st.GetArrivalPrice(), st.GetReleasePrice()) != 0 {
		t.Fatalf("arrival_price %s != release_price %s for an unsliced order — its timing leg "+
			"will report drift that did not happen",
			dec.FromProto(st.GetArrivalPrice()).RatString(),
			dec.FromProto(st.GetReleasePrice()).RatString())
	}
	if !st.GetArrivalAt().AsTime().Equal(st.GetReleaseAt().AsTime()) {
		t.Errorf("arrival_at %s != release_at %s from one observation",
			st.GetArrivalAt().AsTime(), st.GetReleaseAt().AsTime())
	}
}

// A TRADE-ONLY FEED HAS A MARK AND NO WIDTH, and stamping bid == ask == mark
// would report that crossing this market was free. That is the single most
// flattering lie available to an execution report: it pushes the whole cost of
// crossing into the impact residual, the one leg nobody can independently check.
func TestStampRelease_NoQuoteMeansNoWidth(t *testing.T) {
	st := stampWith(t, fakeMarks{price: big.NewRat(64000, 1), asOf: markObserved, seen: true})

	if st.GetReleasePrice() == nil {
		t.Fatal("a trade-only mark must still stamp the release price — the headline shortfall " +
			"needs it and does not need a width")
	}
	if st.GetReleaseBid() != nil || st.GetReleaseAsk() != nil {
		t.Fatalf("a width was stamped for an instrument that was never quoted: bid %v ask %v",
			st.GetReleaseBid(), st.GetReleaseAsk())
	}
}

// THE STAMPED TIME IS THE STALEST COMPONENT'S.
//
// A mark from a trade one second ago and a quote from ten seconds ago are one
// observation with two ages. Recording the newer would describe it as fresher
// than its worst part, and a consumer discounting the cost figure for staleness
// would discount it too little — always in the flattering direction.
func TestStampRelease_TheObservationTimeIsTheOlderOfMarkAndTouch(t *testing.T) {
	st := stampWith(t, fakeMarks{
		price: big.NewRat(64000, 1), asOf: markObserved, seen: true,
		bid: big.NewRat(63990, 1), ask: big.NewRat(64010, 1),
		touchAt: touchObserved, // ten seconds older than the mark
	})

	if got := st.GetReleaseAt().AsTime(); !got.Equal(touchObserved) {
		t.Fatalf("release_at = %s, want the OLDER observation %s — the record claims this "+
			"observation is fresher than its stalest part", got, touchObserved)
	}
}

// A CROSSED QUOTE IS NOT A WIDTH. ask <= bid is a book nobody saw in one
// consistent state, and a negative half-spread arrives in the report as the fund
// being PAID to take liquidity.
func TestStampRelease_ACrossedQuoteIsNotStamped(t *testing.T) {
	st := stampWith(t, fakeMarks{
		price: big.NewRat(64000, 1), asOf: markObserved, seen: true,
		bid: big.NewRat(64010, 1), ask: big.NewRat(63990, 1), touchAt: markObserved,
	})

	if st.GetReleaseBid() != nil || st.GetReleaseAsk() != nil {
		t.Fatalf("a crossed quote was stamped as a width: bid %s ask %s",
			dec.FromProto(st.GetReleaseBid()).RatString(),
			dec.FromProto(st.GetReleaseAsk()).RatString())
	}
}

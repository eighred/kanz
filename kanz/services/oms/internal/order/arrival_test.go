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

var markObserved = time.Date(2026, 8, 1, 11, 59, 30, 0, time.UTC)

func stampWith(t *testing.T, m ArrivalMarks) *orderpb.OrderState {
	t.Helper()
	s := &Service{arrivalMarks: m}
	st := &orderpb.OrderState{InstrumentId: "BTC-USD"}
	s.stampArrival(st)
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
	s.stampArrival(st) // must not panic
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

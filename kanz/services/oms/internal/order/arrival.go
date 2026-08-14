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
}

// WithArrivalMarks supplies the reference-mark source used to stamp an order's
// arrival price at admission. Absent, orders carry no arrival mark and are
// simply not measurable — which is the honest state, not a silent zero.
func WithArrivalMarks(m ArrivalMarks) ServiceOption {
	return func(s *Service) { s.arrivalMarks = m }
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
// IT READS Mark, NOT Lookup, FOR THE VALUE. Lookup returns an entry regardless
// of expiry — its own doc says nothing on a decision path may call it, "or the
// expiry bound is one `if` away from being lost". A stale mark is not an arrival
// price: it is a price from before whatever moved the market, and stamping it
// would bake a fictional benchmark into the record permanently. Lookup is used
// only for the observation time of a mark Mark has already accepted.
//
// THE OBSERVATION TIME IS CARRIED SEPARATELY from accepted_at because the GAP
// between them is itself a cost input: an order benchmarked against a mark from
// thirty seconds ago is being measured against a market that has already moved,
// and a consumer that cannot see the staleness cannot discount the number.
func (s *Service) stampArrival(st *orderpb.OrderState) {
	if s.arrivalMarks == nil || st == nil {
		return
	}
	instrument := st.GetInstrumentId()
	price := s.arrivalMarks.Mark(instrument)
	if price == nil {
		return
	}
	d, ok := dec.ToProtoScaled(price)
	if !ok {
		// Unrepresentable as an exact Decimal. Recording nothing is right: the
		// alternative is a rounded benchmark that reads as exact.
		return
	}
	st.ArrivalPrice = d
	if _, asOf, seen := s.arrivalMarks.Lookup(instrument); seen && !asOf.IsZero() {
		st.ArrivalAt = timestamppb.New(asOf.UTC())
	}
}

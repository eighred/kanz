// Package fillfact is the one statement of what a fill FACT must carry.
//
// # Why it exists
//
// order.order.filled has three independent PRODUCERS (the OMS's own
// order/events.go, and both venue adapters' user-data streams) and six
// consumers, of which TWO fold it into a book of record: the OMS position
// projector and the accounting ledger — the IBOR. Those two are supposed to
// reconcile with each other and with the exchange.
//
// They disagreed about what a valid fill is, and so did the position package
// with itself:
//
//	                          fill_id==""   quantity<=0   venue==""
//	position/postgres.go        refused       refused      refused
//	position/book.go            ACCEPTED      refused      refused
//	accounting ledger.FromFill  ACCEPTED      ACCEPTED     ACCEPTED
//
// THE fill_id ROW IS THE ONE THAT LOSES MONEY SILENTLY. The ledger's entry id
// is "fill:" + FillId and Store.Append is ON CONFLICT (tenant_id, entry_id) DO
// NOTHING. With an empty fill_id the FIRST unidentified fill is journalled and
// every subsequent one in that tenant is discarded as a duplicate — forever, and
// with no error, no DLQ and no counter. The store's own empty-entry-id refusal
// sits two lines above that INSERT and cannot fire, because "fill:" is not
// empty.
//
// Under an unidentified-fill producer the two books therefore diverge in
// OPPOSITE directions: the position book parks the message and stops, the ledger
// accepts one and swallows the rest. Reconciliation reports a break with no way
// to attribute it.
//
// # Why a package rather than a fourth copy
//
// test/arch/cash_subject_agreement_test.go already names this exact set as the
// known-unguarded precedent, in its own doc: "the codebase's existing precedent
// — the fill subjects, which accounting takes from config and the OMS publishes
// from its own literal — is duplication WITHOUT a check. This is the same trade
// with the check added." The check was added for cash and never for fills.
//
// Note which half was copied and which was not: accounting's decodeFill is a
// byte-identical copy of the projector's and says so. The DECODE was mirrored.
// The VALIDATION was added later, to the position store, and did not mirror —
// which is the general shape of "one implementation per concept" failing, since
// the copy is made once and the fix lands on one side afterwards.
package fillfact

import (
	"errors"

	"github.com/eighred/kanz/internal/dec"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// The subjects a fill FACT is published on.
//
// Stated here so the nine literals scattered across producers, consumers and one
// service's config default have a single origin. A subject and its validity
// contract belong together: a consumer that learns the subject from one place
// and the rules from another is how the two drifted.
const (
	SubjectFilled          = "order.order.filled"
	SubjectPartiallyFilled = "order.order.partially_filled"
)

// ErrNotIdentified: a fill with no fill_id cannot be deduplicated, so folding it
// would risk counting the same trade twice — once per pod, or again on a
// redelivery. Every venue stamps one (Binance "symbol-tradeID", OKX
// "instId-tradeId", the sim generates one), so an empty id is a defect in the
// producer, not a case to tolerate. Silently double-counting a fill is how a
// fund's position drifts from the exchange's.
//
// In the LEDGER it is worse than double-counting: see this package's doc — every
// unidentified fill after the first is silently discarded by the entry-id
// conflict clause.
var ErrNotIdentified = errors.New("fill has no fill_id and cannot be folded exactly once")

// ErrHasNoVenue: a fill with no venue cannot be attributed to a holding.
//
// A position sits AT a venue — that is what makes it closable, and what makes
// its collateral segregable (EXEC-M16). order.v1.Fill has always carried the
// venue; a fill arriving without one is a producer defect, and folding it into a
// venue-less bucket would create a holding that no CLOSE could ever reach.
var ErrHasNoVenue = errors.New("fill has no venue — a holding that belongs to no exchange cannot be closed")

// ErrQuantityNotPositive: a fill that moved nothing is a producer defect, and
// folding it PANICS.
//
// foldLot's re-average divides by the new absolute quantity. For a lot a pod has
// not folded before (cur == 0) a zero-quantity fill leaves that divisor at zero,
// and big.Rat.Quo panics `division by zero` — reproduced, and where #217 came
// from. Nothing recovered it, the delivery was never acked, and the broker
// redelivered it into the replacement pod: one malformed fill FACT, and the OMS
// crash-looped estate-wide until someone removed the message by hand.
//
// The order aggregate has always rejected this input — aggregate.go's `fill
// quantity must be > 0`. dec.FromProto(nil) yields zero, so this also covers an
// ABSENT quantity, not just a literal 0.
var ErrQuantityNotPositive = errors.New("fill quantity must be > 0 — a fill that moves nothing cannot be folded")

// Validate reports whether a fill FACT may be folded into a book of record.
//
// REFUSING IS RIGHT RATHER THAN SKIPPING, and the distinction is the whole
// point. A fill that carries none of the information a book needs is a
// misbehaving PRODUCER, and silently ignoring it hides that producer behind a
// healthy-looking consumer — while the other book, which does refuse, parks the
// same message. Callers must return the error so the delivery is nacked and
// parked, not swallow it.
//
// The order of the checks is deliberate: identity first, because an unidentified
// fill is the one whose damage is silent and unattributable afterwards.
func Validate(f *orderpb.Fill) error {
	if f.GetFillId() == "" {
		return ErrNotIdentified
	}
	if f.GetVenue() == "" {
		return ErrHasNoVenue
	}
	if !dec.IsPositive(f.GetQuantity()) {
		return ErrQuantityNotPositive
	}
	return nil
}

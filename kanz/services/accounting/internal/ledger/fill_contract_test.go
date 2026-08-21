package ledger

import (
	"errors"
	"testing"

	"github.com/eighred/kanz/internal/fillfact"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// THE IBOR REFUSES EXACTLY WHAT THE POSITION BOOK REFUSES (#631).
//
// order.order.filled has two consumers that fold it into a book of record, and
// they are meant to reconcile with each other and with the exchange. They
// disagreed about what a valid fill is: the position projector refused three
// shapes by name and this one refused none of them.
//
// Each case below is one of those three, and the fill_id case is the reason this
// is a money bug rather than a tidiness one — see TestFromFillRefusesAnUnidentifiedFill.
func TestFromFillRefusesWhatThePositionBookRefuses(t *testing.T) {
	valid := func() *orderpb.Fill {
		return &orderpb.Fill{
			FillId: "F1", OrderId: "O1", InstrumentId: "AAPL", Venue: "BINANCE",
			Side:       orderpb.Side_SIDE_BUY,
			Quantity:   d(100, 0),
			Price:      d(150, 0),
			Fee:        &commonpb.Money{Amount: d(5, 0), CurrencyCode: "USD"},
			ExecutedAt: timestamppb.New(day(2)),
		}
	}

	cases := []struct {
		name string
		bend func(*orderpb.Fill)
		want error
	}{
		{"no fill_id", func(f *orderpb.Fill) { f.FillId = "" }, fillfact.ErrNotIdentified},
		{"no venue", func(f *orderpb.Fill) { f.Venue = "" }, fillfact.ErrHasNoVenue},
		{"zero quantity", func(f *orderpb.Fill) { f.Quantity = d(0, 0) }, fillfact.ErrQuantityNotPositive},
		// dec.FromProto(nil) yields zero, so an ABSENT quantity is the same defect
		// arriving in a different shape — and the one a producer is likeliest to
		// emit, since a field it never set is absent rather than zero.
		{"absent quantity", func(f *orderpb.Fill) { f.Quantity = nil }, fillfact.ErrQuantityNotPositive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := valid()
			tc.bend(f)
			got, err := FromFill("PF", f, "USD", day(2))
			if !errors.Is(err, tc.want) {
				t.Fatalf("FromFill(%s) error = %v, want %v.\n\n"+
					"The OMS position projector refuses this shape by name. If the ledger books it, "+
					"the two books of record diverge on the same FACT and reconciliation reports a "+
					"break with no way to attribute it.", tc.name, err, tc.want)
			}
			if got != nil {
				t.Fatalf("FromFill(%s) returned an entry alongside its error — a refused fill must "+
					"produce nothing to journal", tc.name)
			}
		})
	}

	// AND THE VALID ONE STILL BOOKS. Without this the three cases above are
	// satisfied by a FromFill that refuses everything.
	if _, err := FromFill("PF", valid(), "USD", day(2)); err != nil {
		t.Fatalf("a well-formed fill was refused: %v", err)
	}
}

// THE UNIDENTIFIED FILL IS THE ONE THAT LOSES MONEY SILENTLY (#631).
//
// This is not the same defect as the other two. EntryID is "fill:" + FillId and
// ledger.Store.Append is ON CONFLICT (tenant_id, entry_id) DO NOTHING — so with
// an empty fill_id every unidentified fill in a tenant collapses onto the SINGLE
// entry id "fill:". The first is journalled; every later one is discarded as a
// duplicate, forever, with no error, no DLQ and no counter.
//
// The store's own empty-entry-id refusal cannot catch it: that check is
// `entry_id == ""` and "fill:" is not empty.
//
// The assertion is on the ENTRY ID rather than on database behaviour, because
// the collision is a property of the id and the conflict clause is documented at
// the INSERT. Proving it here needs no Postgres, so it runs everywhere.
func TestFromFillRefusesAnUnidentifiedFill(t *testing.T) {
	mk := func(id, instrument string) *orderpb.Fill {
		return &orderpb.Fill{
			FillId: id, OrderId: "O1", InstrumentId: instrument, Venue: "BINANCE",
			Side:       orderpb.Side_SIDE_BUY,
			Quantity:   d(1, 0),
			Price:      d(150, 0),
			Fee:        &commonpb.Money{Amount: d(1, 0), CurrencyCode: "USD"},
			ExecutedAt: timestamppb.New(day(2)),
		}
	}

	// Two DIFFERENT trades, both unidentified. Before #631 both were booked
	// through and collided on one entry id.
	for _, instrument := range []string{"AAPL", "MSFT"} {
		if _, err := FromFill("PF", mk("", instrument), "USD", day(2)); !errors.Is(err, fillfact.ErrNotIdentified) {
			t.Fatalf("FromFill with an empty fill_id (%s) error = %v, want ErrNotIdentified", instrument, err)
		}
	}

	// THE COLLISION ITSELF, so the reason the refusal exists is executed rather
	// than only described. Two distinct trades produce the SAME entry id, which
	// the conflict clause then treats as a duplicate.
	a, err := FromFill("PF", mk("X", "AAPL"), "USD", day(2))
	if err != nil {
		t.Fatalf("identified fill: %v", err)
	}
	bEntry, err := FromFill("PF", mk("X", "MSFT"), "USD", day(2))
	if err != nil {
		t.Fatalf("identified fill: %v", err)
	}
	if a.EntryID != bEntry.EntryID {
		t.Fatalf("two fills sharing a fill_id produced different entry ids (%q, %q) — this test's "+
			"premise about the id shape no longer holds, so the refusal above may be guarding "+
			"nothing. Re-derive it from ledger.FromFill before trusting either.", a.EntryID, bEntry.EntryID)
	}
	if a.EntryID != "fill:X" {
		t.Fatalf("entry id = %q, want %q — the id shape this refusal protects has changed", a.EntryID, "fill:X")
	}
}

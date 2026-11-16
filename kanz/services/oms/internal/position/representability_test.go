package position

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
)

// Large finite amounts must fit exactly through coefficient/exponent normalization.
// precision_test.go separately proves explicit refusal beyond that bounded domain.
func largeQty() *big.Rat { return big.NewRat(200_000_000_000, 1) } // $200bn

// A large real quantity must be RESCALED, not WRAPPED. The old dec.ToProto
// would silently truncate this via int64 overflow into a fabricated number;
// dec.ToProtoScaled must preserve the true magnitude instead.
func TestBookStateOf_LargeQuantityRescalesWithoutWrapping(t *testing.T) {
	b := NewBook("USD")
	l := &lot{Qty: largeQty(), AvgCost: big.NewRat(1, 1), Realized: new(big.Rat)}

	st, err := b.stateOf("p1", "XSIM", "AAPL", l, big.NewRat(1, 1), time.Now())
	if err != nil {
		t.Fatalf("stateOf refused a large but real quantity: %v", err)
	}
	got := dec.FromProto(st.GetQuantity())
	if got.Cmp(largeQty()) != 0 {
		t.Fatalf("quantity lost magnitude: got %s want %s (this is what wrapping looks like)",
			dec.Str(got), dec.Str(largeQty()))
	}
}

func TestBookMoney_LargeAmountRescalesWithoutWrapping(t *testing.T) {
	b := NewBook("USD")
	m, err := b.money(largeQty())
	if err != nil {
		t.Fatalf("money refused a large but real amount: %v", err)
	}
	got := dec.FromProto(m.GetAmount())
	if got.Cmp(largeQty()) != 0 {
		t.Fatalf("amount lost magnitude: got %s want %s (this is what wrapping looks like)",
			dec.Str(got), dec.Str(largeQty()))
	}
}

// NON-VACUITY, and the test that matters most: every guard here fails closed, so
// an implementation that refused EVERYTHING would satisfy the "no wrapping"
// tests above by refusing them too. Ordinary values must still produce a state.
func TestBookStateOf_OrdinaryValuesStillSucceed(t *testing.T) {
	b := NewBook("USD")
	l := &lot{Qty: big.NewRat(100, 1), AvgCost: big.NewRat(25, 1), Realized: new(big.Rat)}

	st, err := b.stateOf("p1", "XSIM", "AAPL", l, big.NewRat(30, 1), time.Now())
	if err != nil {
		t.Fatalf("stateOf refused an ordinary 100-share position: %v", err)
	}
	if st.GetQuantity().GetCoefficient() == 0 {
		t.Fatal("quantity is zero for a 100-share position — a zero-valued position is " +
			"dropped by heldPositions and vanishes from the compliance check")
	}
	if st.GetMarketValue().GetAmount().GetCoefficient() == 0 {
		t.Fatal("market value is zero for a 100 × 30 position")
	}
}

// Snapshot must carry large exact quantities and values through the served view.
func TestBookSnapshot_LargePositionRescalesWithoutWrapping(t *testing.T) {
	b := NewBook("USD")
	for k := range b.lots {
		delete(b.lots, k)
	}
	// key is the package's unexported map key; construct it the way book.go does.
	b.lots[key{portfolio: "p1", venue: "XSIM", instrument: "AAPL"}] = &lot{
		Qty: largeQty(), AvgCost: big.NewRat(1, 1), Realized: new(big.Rat),
	}
	snap, err := b.Snapshot(context.Background(), "p1", time.Now())
	if err != nil {
		t.Fatalf("Snapshot refused a large but real position: %v", err)
	}
	if len(snap.GetPositions()) != 1 {
		t.Fatalf("expected 1 position, got %d", len(snap.GetPositions()))
	}
	got := dec.FromProto(snap.GetPositions()[0].GetQuantity())
	if got.Cmp(largeQty()) != 0 {
		t.Fatalf("Snapshot quantity lost magnitude: got %s want %s (this is what wrapping looks like)",
			dec.Str(got), dec.Str(largeQty()))
	}
}

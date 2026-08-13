package position

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/costbasis"
)

// A zero-quantity fill used to PANIC the fold, not merely be wrong (#217).
//
// foldLot's re-average divides by the new absolute quantity. On a lot this pod had
// not folded before, a zero-quantity fill left that divisor at zero and
// big.Rat.Quo panicked `division by zero`. Nothing on the bus path recovered it
// (#218), the delivery was never acked, and the broker redelivered it into the
// replacement pod — one malformed fill FACT crash-looped the OMS estate-wide.
//
// The order aggregate has always rejected this input (aggregate.go, "fill quantity
// must be > 0"). The projector is a second consumer of the same
// order.order.filled subject and did not. These tests pin the two together.
func TestBookApplyRefusesNonPositiveQuantity(t *testing.T) {
	cases := []struct {
		name string
		qty  *commonpb.Decimal
	}{
		{"zero", &commonpb.Decimal{Coefficient: 0, Exponent: 0}},
		{"absent", nil}, // dec.FromProto(nil) yields zero — same divisor, same panic
		{"negative", &commonpb.Decimal{Coefficient: -5, Exponent: 0}},
		{"zero at a finer scale", &commonpb.Decimal{Coefficient: 0, Exponent: -8}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBook("USD")
			got, err := b.Apply(context.Background(), "p1", &orderpb.Fill{
				Venue:        "XBIN",
				InstrumentId: "BTC-USD",
				Quantity:     tc.qty,
				Price:        &commonpb.Decimal{Coefficient: 50000, Exponent: 0},
			}, time.Now())

			if !errors.Is(err, ErrFillQuantityNotPositive) {
				t.Fatalf("Apply(qty=%v) error = %v, want ErrFillQuantityNotPositive.\n"+
					"A fill that moves nothing must be REFUSED so it reaches the DLQ with a "+
					"reason, not folded and not silently skipped", tc.qty, err)
			}
			if got != nil {
				t.Errorf("Apply returned a non-nil Applied alongside the refusal (%+v) — a "+
					"refused fill must produce no position state", got)
			}
		})
	}
}

// A valid fill must still fold, or the guard above is just a way to break the book.
func TestBookApplyStillFoldsAPositiveFill(t *testing.T) {
	b := NewBook("USD")
	got, err := b.Apply(context.Background(), "p1", &orderpb.Fill{
		Venue:        "XBIN",
		InstrumentId: "BTC-USD",
		Quantity:     &commonpb.Decimal{Coefficient: 2, Exponent: 0},
		Price:        &commonpb.Decimal{Coefficient: 50000, Exponent: 0},
	}, time.Now())
	if err != nil {
		t.Fatalf("Apply on a valid fill: %v", err)
	}
	if got == nil || got.Venue == nil {
		t.Fatal("Apply returned no venue state for a valid fill")
	}
}

// The DURABLE store is the one that actually runs: oms-deploy.yaml ships replicas: 2,
// and the in-memory book is explicitly single-replica-only. Guarding Book.Apply and
// leaving Postgres.Apply open would fix the path nothing uses.
//
// Gated on TEST_POSTGRES_URL, so it skips on a bare checkout — which is exactly why
// the Book cases above are not gated.
func TestPostgresApplyRefusesNonPositiveQuantity(t *testing.T) {
	pool := newPool(t, "acme") // t.Skip inside when TEST_POSTGRES_URL is unset
	store := NewPostgres(pool, "USD")

	got, err := store.Apply(context.Background(), "p1", &orderpb.Fill{
		FillId:       "fill-zero-1",
		Venue:        "XBIN",
		InstrumentId: "BTC-USD",
		Quantity:     &commonpb.Decimal{Coefficient: 0, Exponent: 0},
		Price:        &commonpb.Decimal{Coefficient: 50000, Exponent: 0},
	}, time.Now())

	if !errors.Is(err, ErrFillQuantityNotPositive) {
		t.Fatalf("Apply error = %v, want ErrFillQuantityNotPositive", err)
	}
	if got != nil {
		t.Errorf("Apply returned %+v alongside the refusal — a refused fill must write "+
			"no position state", got)
	}
}

// foldLot is the ONE fold shared by the in-memory and durable books, and its
// divisor is derived rather than validated. Both callers now refuse a non-positive
// fill, so this is unreachable through Apply — which is exactly why it is worth
// pinning directly: the next caller does not inherit those guards, and the failure
// mode of this line is taking the process down.
func TestFoldLotDoesNotPanicWhenTheLotNetsToZero(t *testing.T) {
	l := zeroLot()

	// Straight to zero from flat — the shape that panicked.
	costbasis.Fold(l, new(big.Rat), big.NewRat(50000, 1))
	if l.Qty.Sign() != 0 {
		t.Errorf("qty = %s, want 0", l.Qty.RatString())
	}
	if l.AvgCost.Sign() != 0 {
		t.Errorf("a flat lot has no basis; avg = %s, want 0", l.AvgCost.RatString())
	}

	// Open, then close exactly — nets to zero through the opposite-direction arm.
	costbasis.Fold(l, big.NewRat(3, 1), big.NewRat(100, 1))
	costbasis.Fold(l, big.NewRat(-3, 1), big.NewRat(120, 1))
	if l.Qty.Sign() != 0 {
		t.Errorf("after a full close, qty = %s, want 0", l.Qty.RatString())
	}
}

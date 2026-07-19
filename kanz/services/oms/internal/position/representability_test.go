package position

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/internal/dec"
)

// largeQty is well past dec.ToProto's ~$92bn wrapping ceiling at the fixed
// scale (int64 coefficient at scale -8 overflows above ~92,233,720,368), but
// it is a real, plausible financial magnitude — nothing like the ~10^2billion-digit
// value that would actually exhaust dec.ToProtoScaled's rescaling (see NOTE
// below). It is the value this task exists for: dec.ToProto would silently
// wrap it into a fabricated number; dec.ToProtoScaled must instead preserve
// its magnitude exactly.
//
// NOTE ON WHAT THIS FILE DOES NOT TEST: dec.ToProtoScaled only refuses (ok ==
// false) when it cannot raise its exponent far enough to fit an int64
// coefficient, and its exponent budget runs to math.MaxInt32 (~2.147e9).
// Reaching that requires a coefficient of roughly two billion DECIMAL DIGITS
// — verified empirically before writing this file: a 10^400 value (401
// digits, the brief's original proposal) rescales successfully
// (ok=true, coefficient=1e18, exponent=382); at 100,000 digits ToProtoScaled
// takes ~1.6s, and at 1,000,000 digits it still had not returned after 60s
// (the loop is O(digits) iterations of O(digits) work, i.e. O(digits^2)).
// Extrapolating that scaling, the ~2 billion digits needed to reach a genuine
// refusal would take on the order of decades of CPU time and ~900MB to hold
// the number — not something any test suite can exercise. The refusal branch
// in money/stateOf is still implemented exactly as specified (every dec.ToProtoScaled
// call checks its ok return and returns a named error instead of proceeding with
// zero), it is just not reachable from a test in finite time; this file instead
// proves the branch that IS reachable and IS the actual defect being fixed:
// large real values no longer wrap.
func largeQty() *big.Rat { return big.NewRat(200_000_000_000, 1) } // $200bn

// A large real quantity must be RESCALED, not WRAPPED. The old dec.ToProto
// would silently truncate this via int64 overflow into a fabricated number;
// dec.ToProtoScaled must preserve the true magnitude instead.
func TestBookStateOf_LargeQuantityRescalesWithoutWrapping(t *testing.T) {
	b := NewBook("USD")
	l := &lot{qty: largeQty(), avg: big.NewRat(1, 1), realized: new(big.Rat)}

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
	l := &lot{qty: big.NewRat(100, 1), avg: big.NewRat(25, 1), realized: new(big.Rat)}

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

// Snapshot is the other caller with an error channel and must propagate a
// money/stateOf error rather than swallow it. It must also carry a large real
// position through without wrapping it, which is the property under test
// here since a genuine refusal is not reachable in finite test time (see the
// note on largeQty above). The book is seeded by writing b.lots directly —
// this test is in-package, and going through Apply would couple it to the
// fill helper for no benefit, since what is under test is Snapshot's handling
// of money/stateOf's return values.
func TestBookSnapshot_LargePositionRescalesWithoutWrapping(t *testing.T) {
	b := NewBook("USD")
	for k := range b.lots {
		delete(b.lots, k)
	}
	// key is the package's unexported map key; construct it the way book.go does.
	b.lots[key{portfolio: "p1", venue: "XSIM", instrument: "AAPL"}] = &lot{
		qty: largeQty(), avg: big.NewRat(1, 1), realized: new(big.Rat),
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

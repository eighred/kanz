package projection

import (
	"math/big"
	"testing"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

func ex(side orderpb.Side, qty, price int64, instrument string) execution {
	return execution{
		instrument: instrument, side: side,
		qty: big.NewRat(qty, 1), price: big.NewRat(price, 1), fee: new(big.Rat),
	}
}

func rat(n int64) *big.Rat { return big.NewRat(n, 1) }

func TestFold_WeightedAverageEntry(t *testing.T) {
	pos := foldPositions([]execution{
		ex(orderpb.Side_SIDE_BUY, 1, 100, "BTC"),
		ex(orderpb.Side_SIDE_BUY, 1, 200, "BTC"),
	})["BTC"]
	if pos.net.Cmp(rat(2)) != 0 {
		t.Errorf("net = %s, want 2", pos.net.RatString())
	}
	if pos.avg.Cmp(rat(150)) != 0 { // (100+200)/2
		t.Errorf("avg = %s, want 150", pos.avg.RatString())
	}
	if pos.realized.Sign() != 0 {
		t.Errorf("realized = %s, want 0 (no close yet)", pos.realized.RatString())
	}
}

func TestFold_ReduceRealizesAgainstAverage(t *testing.T) {
	pos := foldPositions([]execution{
		ex(orderpb.Side_SIDE_BUY, 1, 100, "BTC"),
		ex(orderpb.Side_SIDE_BUY, 1, 200, "BTC"),  // avg 150, net 2
		ex(orderpb.Side_SIDE_SELL, 1, 250, "BTC"), // close 1 @ (250-150)=100
	})["BTC"]
	if pos.net.Cmp(rat(1)) != 0 {
		t.Errorf("net = %s, want 1", pos.net.RatString())
	}
	if pos.avg.Cmp(rat(150)) != 0 { // average unchanged on a reduce
		t.Errorf("avg = %s, want 150", pos.avg.RatString())
	}
	if pos.realized.Cmp(rat(100)) != 0 {
		t.Errorf("realized = %s, want 100", pos.realized.RatString())
	}
}

func TestFold_FlipThroughZeroRebasesAverage(t *testing.T) {
	pos := foldPositions([]execution{
		ex(orderpb.Side_SIDE_BUY, 1, 100, "BTC"),  // long 1 @ 100
		ex(orderpb.Side_SIDE_SELL, 3, 200, "BTC"), // close 1 @ (200-100)=100, flip to short 2 @ 200
	})["BTC"]
	if pos.net.Cmp(rat(-2)) != 0 {
		t.Errorf("net = %s, want -2 (flipped short)", pos.net.RatString())
	}
	if pos.avg.Cmp(rat(200)) != 0 { // re-based at the crossing price
		t.Errorf("avg = %s, want 200", pos.avg.RatString())
	}
	if pos.realized.Cmp(rat(100)) != 0 {
		t.Errorf("realized = %s, want 100", pos.realized.RatString())
	}
}

func TestFold_ShortThenCoverRealizes(t *testing.T) {
	pos := foldPositions([]execution{
		ex(orderpb.Side_SIDE_SELL, 1, 100, "BTC"), // short 1 @ 100
		ex(orderpb.Side_SIDE_BUY, 1, 80, "BTC"),   // cover @ 80 → realized (100-80)=20
	})["BTC"]
	if pos.net.Sign() != 0 {
		t.Errorf("net = %s, want 0 (flat)", pos.net.RatString())
	}
	if pos.realized.Cmp(rat(20)) != 0 {
		t.Errorf("realized = %s, want 20 (short profit)", pos.realized.RatString())
	}
}

func TestFold_FeesReduceRealized(t *testing.T) {
	e1 := ex(orderpb.Side_SIDE_BUY, 1, 100, "BTC")
	e1.fee = big.NewRat(1, 1) // $1 fee
	e2 := ex(orderpb.Side_SIDE_SELL, 1, 100, "BTC")
	e2.fee = big.NewRat(1, 1)
	pos := foldPositions([]execution{e1, e2})["BTC"]
	// Round-trip flat at same price: gross P&L 0, minus $2 fees = -2.
	if pos.realized.Cmp(big.NewRat(-2, 1)) != 0 {
		t.Errorf("realized = %s, want -2 (fees)", pos.realized.RatString())
	}
}

func TestFold_UnrealizedFromMark(t *testing.T) {
	pos := foldPositions([]execution{ex(orderpb.Side_SIDE_BUY, 2, 100, "BTC")})["BTC"]
	u := pos.unrealized(rat(150)) // net 2 × (150-100) = 100
	if u == nil || u.Cmp(rat(100)) != 0 {
		t.Errorf("unrealized = %v, want 100", u)
	}
	if pos.unrealized(nil) != nil {
		t.Error("unrealized with no mark must be nil (degrade, don't fabricate)")
	}
}

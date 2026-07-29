package book

import (
	"math/big"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/dec"
)

func d(s string) *commonpb.Decimal {
	r, _ := new(big.Rat).SetString(s)
	return dec.ToProto(r)
}

func lvl(price, size string) *marketpb.PriceLevel {
	return &marketpb.PriceLevel{Price: d(price), Size: d(size)}
}

func seedBook(t *testing.T) *Book {
	t.Helper()
	b := New("BTC-USD", "BTCUSDT", "BINANCE")
	b.ApplySnapshot(&marketpb.OrderBookSnapshot{
		InstrumentId:       "BTC-USD",
		LastUpdateSequence: 100,
		Bids:               []*marketpb.PriceLevel{lvl("50000", "1"), lvl("49999", "2")},
		Asks:               []*marketpb.PriceLevel{lvl("50001", "1"), lvl("50002", "3")},
	})
	return b
}

func TestApplyDelta_FoldsAndOrders(t *testing.T) {
	b := seedBook(t)
	// Chain a delta: improve the bid, add a new ask level.
	if err := b.ApplyDelta(&marketpb.OrderBookDelta{
		PrevUpdateSequence: 100, LastUpdateSequence: 101,
		Bids: []*marketpb.PriceLevel{lvl("50000", "5")},
		Asks: []*marketpb.PriceLevel{lvl("50000.5", "2")},
	}); err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}

	snap := b.Snapshot(10)
	if snap.GetLastUpdateSequence() != 101 {
		t.Fatalf("last_update_sequence = %d, want 101", snap.GetLastUpdateSequence())
	}
	// Bids strictly descending; best bid updated size 5.
	if got := dec.FromProto(snap.GetBids()[0].GetPrice()); got.Cmp(big.NewRat(50000, 1)) != 0 {
		t.Fatalf("best bid price = %s, want 50000", got.RatString())
	}
	if got := dec.FromProto(snap.GetBids()[0].GetSize()); got.Cmp(big.NewRat(5, 1)) != 0 {
		t.Fatalf("best bid size = %s, want 5 (delta replaced level)", got.RatString())
	}
	// Asks strictly ascending; new level 50000.5 is now the best ask.
	if got := dec.FromProto(snap.GetAsks()[0].GetPrice()); got.Cmp(new(big.Rat).SetFrac64(1000010, 20)) != 0 {
		t.Fatalf("best ask = %s, want 50000.5", got.RatString())
	}
	for i := 1; i < len(snap.GetAsks()); i++ {
		if dec.FromProto(snap.GetAsks()[i-1].GetPrice()).Cmp(dec.FromProto(snap.GetAsks()[i].GetPrice())) >= 0 {
			t.Fatal("asks not strictly ascending")
		}
	}
}

func TestApplyDelta_SizeZeroRemovesLevel(t *testing.T) {
	b := seedBook(t)
	if err := b.ApplyDelta(&marketpb.OrderBookDelta{
		PrevUpdateSequence: 100, LastUpdateSequence: 101,
		Bids: []*marketpb.PriceLevel{lvl("49999", "0")}, // remove the second bid
	}); err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	snap := b.Snapshot(10)
	if len(snap.GetBids()) != 1 {
		t.Fatalf("bids = %d, want 1 (level removed by size 0)", len(snap.GetBids()))
	}
	if got := dec.FromProto(snap.GetBids()[0].GetPrice()); got.Cmp(big.NewRat(50000, 1)) != 0 {
		t.Fatalf("remaining bid = %s, want 50000", got.RatString())
	}
}

func TestApplyDelta_SequenceGapRefused(t *testing.T) {
	b := seedBook(t) // lastSeq = 100
	err := b.ApplyDelta(&marketpb.OrderBookDelta{
		PrevUpdateSequence: 105, LastUpdateSequence: 106, // does not chain off 100
		Bids: []*marketpb.PriceLevel{lvl("50000", "9")},
	})
	if err != ErrSequenceGap {
		t.Fatalf("err = %v, want ErrSequenceGap", err)
	}
	// The out-of-order delta must NOT have mutated the book.
	snap := b.Snapshot(10)
	if got := dec.FromProto(snap.GetBids()[0].GetSize()); got.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("best bid size = %s, want 1 (gap delta must not fold)", got.RatString())
	}
	if snap.GetLastUpdateSequence() != 100 {
		t.Fatalf("last_update_sequence = %d, want 100 (unchanged)", snap.GetLastUpdateSequence())
	}
}

func TestSnapshot_DepthBounded(t *testing.T) {
	b := New("BTC-USD", "BTCUSDT", "BINANCE")
	b.ApplySnapshot(&marketpb.OrderBookSnapshot{
		Bids: []*marketpb.PriceLevel{lvl("100", "1"), lvl("99", "1"), lvl("98", "1")},
		Asks: []*marketpb.PriceLevel{lvl("101", "1"), lvl("102", "1"), lvl("103", "1")},
	})
	snap := b.Snapshot(2)
	if len(snap.GetBids()) != 2 || len(snap.GetAsks()) != 2 {
		t.Fatalf("bounded snapshot = %d bids / %d asks, want 2/2", len(snap.GetBids()), len(snap.GetAsks()))
	}
	// Depth keeps the BEST levels.
	if got := dec.FromProto(snap.GetBids()[0].GetPrice()); got.Cmp(big.NewRat(100, 1)) != 0 {
		t.Fatalf("top bid after truncation = %s, want 100", got.RatString())
	}
}

func TestBestBidAsk(t *testing.T) {
	b := seedBook(t)
	if b.BestBid().Cmp(big.NewRat(50000, 1)) != 0 {
		t.Fatalf("best bid = %s, want 50000", b.BestBid().RatString())
	}
	if b.BestAsk().Cmp(big.NewRat(50001, 1)) != 0 {
		t.Fatalf("best ask = %s, want 50001", b.BestAsk().RatString())
	}
	empty := New("X", "X", "SIM")
	if empty.BestBid() != nil || empty.BestAsk() != nil {
		t.Fatal("empty book must report nil best bid/ask")
	}
}

func TestUnsequencedFeedSkipsGapCheck(t *testing.T) {
	// A sim/unsequenced feed carries all-zero sequences; the gap check is skipped.
	b := New("BTC-USD", "BTCUSDT", "SIM")
	b.ApplySnapshot(&marketpb.OrderBookSnapshot{Bids: []*marketpb.PriceLevel{lvl("100", "1")}})
	if err := b.ApplyDelta(&marketpb.OrderBookDelta{Bids: []*marketpb.PriceLevel{lvl("100", "7")}}); err != nil {
		t.Fatalf("unsequenced delta should fold, got %v", err)
	}
	if got := dec.FromProto(b.Snapshot(10).GetBids()[0].GetSize()); got.Cmp(big.NewRat(7, 1)) != 0 {
		t.Fatalf("size = %s, want 7", got.RatString())
	}
}

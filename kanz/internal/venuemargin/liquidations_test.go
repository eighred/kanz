package venuemargin

import (
	"math/big"
	"testing"
	"time"

	collateralpb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// THE ATTRIBUTION MUST SURVIVE THE FOLD (#408 control 4).
//
// #708 put instrument_id on VenueLiquidationPrice so a liquidation price could
// be paired with a mark. It reached the wire and this fold DROPPED it: the
// snapshot was keyed by venue symbol and stored a bare price, so the one
// consumer that reads the message discarded the only field that makes it usable.
// A field that is published and never retained is worse than one that was never
// added — it reads, in the schema and in the adapter, as though the pairing
// works.
func TestAttributionSurvivesTheFold(t *testing.T) {
	v := New(WithClock(frozen))
	fold(t, v, state(t)) // BTC-USDT-SWAP, unattributed by the fixture

	msg := state(t)
	msg.LiquidationPrices = []*collateralpb.VenueLiquidationPrice{
		{VenueSymbol: "BTC-USDT-SWAP", InstrumentId: "BTC-PERP", Price: decimal(t, "41000")},
	}
	fold(t, v, msg)

	got, ok := v.Liquidations("OKX", "acct-1")
	if !ok {
		t.Fatal("Liquidations reported UNKNOWN for an account observed a moment ago")
	}
	if len(got) != 1 {
		t.Fatalf("got %d positions, want 1: %+v", len(got), got)
	}
	if got[0].InstrumentID != "BTC-PERP" {
		t.Errorf("instrument attribution was lost in the fold: got %q, want %q — the liquidation "+
			"price cannot be paired with a mark without it", got[0].InstrumentID, "BTC-PERP")
	}
	if got[0].VenueSymbol != "BTC-USDT-SWAP" {
		t.Errorf("venue symbol = %q", got[0].VenueSymbol)
	}
	if got[0].Price.Value().Cmp(rat(t, "41000")) != 0 {
		t.Errorf("price = %v", got[0].Price.Value())
	}
}

// AN UNATTRIBUTED POSITION IS ENUMERATED, NOT DROPPED. The fund holds a
// leveraged position this deployment cannot name; hiding it would make the book
// look smaller than it is, which is the direction that gets someone hurt. The
// consumer sees an empty InstrumentID and the account's Coverage says why.
func TestAnUnattributedPositionIsStillEnumerated(t *testing.T) {
	v := New(WithClock(frozen))
	msg := state(t)
	msg.LiquidationPrices = []*collateralpb.VenueLiquidationPrice{
		{VenueSymbol: "BTC-USDT-SWAP", InstrumentId: "BTC-PERP", Price: decimal(t, "41000")},
		{VenueSymbol: "MYSTERY-SWAP", Price: decimal(t, "7")},
	}
	msg.Coverage = &domainpb.InputCoverage{
		Contributed:   1,
		ExcludedCount: 1,
		Exclusions:    []*domainpb.InputExclusion{{InstrumentId: "MYSTERY-SWAP", Reason: SkipUnmappedVenueSymbol}},
	}
	fold(t, v, msg)

	got, ok := v.Liquidations("OKX", "acct-1")
	if !ok || len(got) != 2 {
		t.Fatalf("got %d positions (ok=%v), want both: %+v", len(got), ok, got)
	}
	// Ordered by venue symbol: BTC-USDT-SWAP, then MYSTERY-SWAP.
	if got[1].VenueSymbol != "MYSTERY-SWAP" || got[1].InstrumentID != "" {
		t.Errorf("unattributed position = %+v, want MYSTERY-SWAP with an empty InstrumentID", got[1])
	}
	cov, ok := v.Coverage("OKX", "acct-1")
	if !ok || cov.ExcludedCount() != 1 {
		t.Errorf("coverage did not carry the exclusion that explains the empty attribution: %+v", cov)
	}
}

// UNKNOWN AND FLAT ARE DIFFERENT ANSWERS, and this is the pair the measure
// downstream turns on: ok=false forbids any conclusion, while ok=true with no
// positions is the exchange saying the account holds nothing leveraged — the one
// case where an exact-zero proximity is honest.
func TestUnknownIsNotFlat(t *testing.T) {
	v := New(WithClock(frozen))

	if _, ok := v.Liquidations("OKX", "never-seen"); ok {
		t.Error("an account that was never observed answered ok=true — a caller may now conclude it " +
			"holds nothing leveraged, which nobody has checked")
	}

	msg := state(t)
	msg.LiquidationPrices = nil
	fold(t, v, msg)
	got, ok := v.Liquidations("OKX", "acct-1")
	if !ok {
		t.Fatal("an observed account with no open positions answered UNKNOWN — that is the exchange's " +
			"positive statement being thrown away")
	}
	if len(got) != 0 {
		t.Errorf("got %d positions, want none", len(got))
	}
}

// A STALE OBSERVATION IS UNKNOWN, on the same rule as every other reader. This
// is what makes currentSnapshot worth factoring: an enumerator that forgot the
// freshness check would serve a dead feed's positions to the risk measure while
// the quantity lookups refused them.
func TestStaleLiquidationsAreUnknown(t *testing.T) {
	var staleCalls int
	v := New(
		WithClock(func() time.Time { return observed.Add(time.Hour) }),
		WithMaxAge(time.Minute),
		WithOnStale(func(string, string, time.Duration) { staleCalls++ }),
	)
	fold(t, v, state(t))

	if _, ok := v.Liquidations("OKX", "acct-1"); ok {
		t.Error("a stale observation was enumerated — the risk measure would read a liquidation " +
			"boundary from a feed that stopped an hour ago")
	}
	if staleCalls == 0 {
		t.Error("the stale hook did not fire, so an operator sees no signal for this read")
	}
}

// THE CALLER CANNOT REACH BACK INTO THE FOLD. A slice looks borrowable in a way
// a scalar does not, and big.Rat is a mutable pointer type: without the copy, a
// consumer doing arithmetic in place would silently rewrite the shared view that
// every margin control reads.
func TestLiquidationsDoNotAliasTheFold(t *testing.T) {
	v := New(WithClock(frozen))
	msg := state(t)
	msg.LiquidationPrices = []*collateralpb.VenueLiquidationPrice{
		{VenueSymbol: "BTC-USDT-SWAP", InstrumentId: "BTC-PERP", Price: decimal(t, "41000")},
	}
	fold(t, v, msg)

	// Asserting through Value() would prove NOTHING here: it copies on the way
	// out, so the fold survives a caller's arithmetic whether or not Liquidations
	// copied. This test is in-package precisely so it can compare the pointers
	// and see the sharing itself.
	held := v.byAcct[accountKey{venue: "OKX", account: "acct-1"}].liquidation["BTC-USDT-SWAP"].price
	got, _ := v.Liquidations("OKX", "acct-1")
	if got[0].Price.value == held {
		t.Fatal("Liquidations handed out the fold's own *big.Rat — one caller's in-place Add would " +
			"rewrite the liquidation price every margin control reads")
	}

	got[0].Price.value.Add(got[0].Price.value, big.NewRat(1_000_000, 1))
	again, _ := v.Liquidations("OKX", "acct-1")
	if again[0].Price.Value().Cmp(rat(t, "41000")) != 0 {
		t.Fatalf("the fold was mutated through the returned slice: now %v", again[0].Price.Value())
	}
}

// ORDERING IS DETERMINISTIC. The measure downstream reports the WORST proximity;
// on a tie, map iteration order would make which position is named vary between
// two reads of one observation.
func TestLiquidationsAreOrderedByVenueSymbol(t *testing.T) {
	v := New(WithClock(frozen))
	msg := state(t)
	msg.LiquidationPrices = []*collateralpb.VenueLiquidationPrice{
		{VenueSymbol: "SOL-USDT-SWAP", InstrumentId: "SOL-PERP", Price: decimal(t, "90")},
		{VenueSymbol: "BTC-USDT-SWAP", InstrumentId: "BTC-PERP", Price: decimal(t, "41000")},
		{VenueSymbol: "ETH-USDT-SWAP", InstrumentId: "ETH-PERP", Price: decimal(t, "2200")},
	}
	msg.ObservedAt = timestamppb.New(observed)
	fold(t, v, msg)

	want := []string{"BTC-USDT-SWAP", "ETH-USDT-SWAP", "SOL-USDT-SWAP"}
	for i := 0; i < 20; i++ {
		got, ok := v.Liquidations("OKX", "acct-1")
		if !ok || len(got) != 3 {
			t.Fatalf("got %d (ok=%v)", len(got), ok)
		}
		for j, w := range want {
			if got[j].VenueSymbol != w {
				t.Fatalf("read %d position %d = %q, want %q — map order is leaking into the answer",
					i, j, got[j].VenueSymbol, w)
			}
		}
	}
}

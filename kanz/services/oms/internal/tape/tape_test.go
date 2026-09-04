package tape

import (
	"context"
	"math/big"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
)

// WHAT THIS FOLD MAY AND MAY NOT VOUCH FOR (#1007).
//
// Every case here is about the boundary between "this many units printed" and "I
// cannot say", because that boundary is the whole value of the fold: a
// participation rate is only as honest as its denominator, and a denominator
// this platform invented would be loudest in exactly the thin market a
// participation cap exists for.

var tapeT0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// candleAt builds the wire message market-ingest publishes for one minute.
func candleAt(instrument, venue string, open time.Time, volume int64) []byte {
	return candleWith(instrument, venue, open, open.Add(time.Minute), &commonpb.Decimal{
		Coefficient: volume, Exponent: 0,
	})
}

func candleWith(instrument, venue string, open, close time.Time, vol *commonpb.Decimal) []byte {
	bar := &marketpb.Bar{Volume: vol}
	if !open.IsZero() {
		bar.OpenTime = timestamppb.New(open)
	}
	if !close.IsZero() {
		bar.CloseTime = timestamppb.New(close)
	}
	b, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: instrument,
		Mic:          venue,
		EventTime:    timestamppb.New(close),
		Data:         &marketpb.MarketDataEvent_Bar{Bar: bar},
	})
	if err != nil {
		panic(err)
	}
	return b
}

func fold(t *testing.T, f *Fold, payload []byte) {
	t.Helper()
	if err := f.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("Handle returned %v — this fold observes and owns nothing, so every delivery "+
			"must be acked or one unreadable message stalls the subscription behind it", err)
	}
}

func mustVolume(t *testing.T, f *Fold, from, to time.Time) *big.Rat {
	t.Helper()
	v, ok := f.Volume("BTC-USD", "XSIM", from, to)
	if !ok {
		t.Fatalf("Volume(%s, %s) is UNKNOWN, want a measured quantity", from, to)
	}
	return v
}

// A WINDOW IS THE SUM OF THE CANDLES THAT OVERLAP IT.
//
// Not the candles CONTAINED by it: a slice interval is the operator's window
// divided by their slice count and lands on a minute boundary only by accident,
// so containment would answer zero for every interval shorter than a minute —
// and answering zero is the one thing this must never do.
func TestVolume_SumsTheCandlesCoveringTheWindow(t *testing.T) {
	f := NewFold(0)
	for i, v := range []int64{10, 20, 30} {
		fold(t, f, candleAt("BTC-USD", "XSIM", tapeT0.Add(time.Duration(i)*time.Minute), v))
	}

	// The whole three minutes.
	if got := mustVolume(t, f, tapeT0, tapeT0.Add(3*time.Minute)).RatString(); got != "60" {
		t.Errorf("volume over three minutes = %s, want 60", got)
	}
	// A window that starts on a boundary and ends inside the third minute still
	// counts the third candle whole — which is why the rate built on this is
	// documented as a LOWER bound rather than as the rate.
	if got := mustVolume(t, f, tapeT0, tapeT0.Add(2*time.Minute+30*time.Second)).RatString(); got != "60" {
		t.Errorf("volume over 2m30s = %s, want 60 — a partly-covered minute is counted whole, so "+
			"the denominator is never short and the rate is never overstated", got)
	}
	// And a window entirely inside one minute is that minute.
	if got := mustVolume(t, f, tapeT0.Add(70*time.Second), tapeT0.Add(80*time.Second)).RatString(); got != "20" {
		t.Errorf("volume over ten seconds inside the second minute = %s, want 20", got)
	}
}

// A WINDOW THAT STARTS BEFORE THE FOLD WAS WATCHING IS UNKNOWN.
//
// This is the fold's entire attestation and its only honest one: it knows when
// its own earliest retained candle opened, and it cannot speak for anything
// before that. A cold pod that has folded ten minutes and is asked about an hour
// would otherwise return the ten minutes it has — a real number, for a window
// fifty minutes of which nobody watched, and the missing part is silent.
func TestVolume_AWindowBeforeTheEarliestCandleIsUnknown(t *testing.T) {
	f := NewFold(0)
	fold(t, f, candleAt("BTC-USD", "XSIM", tapeT0, 10))

	if v, ok := f.Volume("BTC-USD", "XSIM", tapeT0.Add(-time.Hour), tapeT0.Add(time.Minute)); ok {
		t.Fatalf("volume = %s for a window starting an hour before the first candle this fold "+
			"ever saw. That number is the ten minutes it happened to hold, presented as an hour",
			v.RatString())
	}
	// The same window starting exactly at the earliest candle IS answerable — the
	// bound is "before", not "at", or a fold could never speak for its own first
	// minute.
	if _, ok := f.Volume("BTC-USD", "XSIM", tapeT0, tapeT0.Add(time.Minute)); !ok {
		t.Error("a window starting exactly at the earliest retained candle is UNKNOWN — the fold " +
			"cannot vouch for the one minute it definitely watched")
	}
}

// A GAP INSIDE THE RETAINED RANGE IS UNKNOWN RATHER THAN ZERO.
//
// internal/marketedge/bars emits no candle for a minute in which nothing traded
// and cannot tell that from a feed that was down. A window covered by no candle
// therefore has no volume, not a volume of zero, and the caller counts it as
// coverage rather than dividing by it.
func TestVolume_AWindowWithNoCandleIsUnknown(t *testing.T) {
	f := NewFold(0)
	fold(t, f, candleAt("BTC-USD", "XSIM", tapeT0, 10))
	fold(t, f, candleAt("BTC-USD", "XSIM", tapeT0.Add(10*time.Minute), 10))

	from, to := tapeT0.Add(3*time.Minute), tapeT0.Add(5*time.Minute)
	if v, ok := f.Volume("BTC-USD", "XSIM", from, to); ok {
		t.Fatalf("volume = %s for two minutes no candle covers. A quiet market and a dead feed "+
			"look identical from here, and %s of them is a participation denominator this "+
			"platform invented", v.RatString(), v.RatString())
	}
}

// A SERIES IS ONE BOOK. Being 8% of Binance and 40% of OKX are different facts
// about the same order, and a fold that merged them would answer the flattering
// one — the direction a participation control must never fail in.
func TestVolume_ASeriesIsPerVenue(t *testing.T) {
	f := NewFold(0)
	fold(t, f, candleAt("BTC-USD", "BINANCE", tapeT0, 1000))

	if v, ok := f.Volume("BTC-USD", "OKX", tapeT0, tapeT0.Add(time.Minute)); ok {
		t.Fatalf("OKX volume = %s, taken from a BINANCE candle", v.RatString())
	}
	if _, ok := f.Volume("ETH-USD", "BINANCE", tapeT0, tapeT0.Add(time.Minute)); ok {
		t.Fatal("an ETH window was answered from a BTC candle")
	}
}

// A REDELIVERED CANDLE REPLACES, IT DOES NOT ACCUMULATE.
//
// SubscribeReplay folds the whole retained history and a redelivery is ordinary.
// bars refuses to restate a published candle, so a second message for one minute
// is the same candle again — and ADDING it would double a denominator and report
// a participation HALF what it was, which is the flattering direction and the one
// nobody would question.
func TestVolume_ARedeliveredCandleIsIdempotent(t *testing.T) {
	f := NewFold(0)
	for range 3 {
		fold(t, f, candleAt("BTC-USD", "XSIM", tapeT0, 10))
	}
	if got := mustVolume(t, f, tapeT0, tapeT0.Add(time.Minute)).RatString(); got != "10" {
		t.Errorf("volume after three deliveries of one candle = %s, want 10 — a replay would "+
			"halve every participation rate this OMS reports", got)
	}
}

// THE HORIZON IS MEASURED FROM THE DATA, AND EVICTION MOVES THE COVERAGE FLOOR
// WITH IT.
//
// The second half is the one that matters: dropping a candle without raising what
// this fold will vouch for would leave it answering a confident sum for a window
// whose early minutes it has just thrown away.
func TestVolume_EvictionRaisesTheCoverageFloor(t *testing.T) {
	f := NewFold(2 * time.Minute)
	fold(t, f, candleAt("BTC-USD", "XSIM", tapeT0, 10))
	fold(t, f, candleAt("BTC-USD", "XSIM", tapeT0.Add(time.Minute), 20))
	if got := mustVolume(t, f, tapeT0, tapeT0.Add(2*time.Minute)).RatString(); got != "30" {
		t.Fatalf("precondition: volume = %s, want 30", got)
	}

	// A candle five minutes on pushes the horizon past the first two.
	fold(t, f, candleAt("BTC-USD", "XSIM", tapeT0.Add(5*time.Minute), 5))

	if v, ok := f.Volume("BTC-USD", "XSIM", tapeT0, tapeT0.Add(2*time.Minute)); ok {
		t.Fatalf("volume = %s over a window whose candles were evicted — the fold is vouching "+
			"for minutes it no longer holds", v.RatString())
	}
	if got := mustVolume(t, f, tapeT0.Add(5*time.Minute), tapeT0.Add(6*time.Minute)).RatString(); got != "5" {
		t.Errorf("the retained candle reads %s, want 5", got)
	}
	if _, _, evicted, series := f.Stats(); evicted == 0 || series != 1 {
		t.Errorf("stats: evicted=%d series=%d, want evicted>0 and one series retained", evicted, series)
	}
}

// A SERIES THAT GOES QUIET PAST THE HORIZON LEAVES THE MAP ENTIRELY.
//
// The key space comes off the wire, so a producer that published a typo'd
// instrument once would otherwise leave an entry behind for the life of the pod —
// the leak shape #805 was filed for, and what
// test/arch/long_lived_maps_are_evicted_test.go exists to refuse.
func TestFold_ADeadSeriesIsDropped(t *testing.T) {
	f := NewFold(2 * time.Minute)
	fold(t, f, candleAt("BTC-USD", "XSIM", tapeT0, 10))
	fold(t, f, candleAt("TYPO-USD", "XSIM", tapeT0, 10))
	if _, _, _, series := f.Stats(); series != 2 {
		t.Fatalf("precondition: %d series, want 2", series)
	}

	for i := range 5 {
		fold(t, f, candleAt("BTC-USD", "XSIM", tapeT0.Add(time.Duration(10+i)*time.Minute), 1))
	}
	if _, _, _, series := f.Stats(); series != 1 {
		t.Errorf("%d series retained, want 1 — the quiet series' candles aged out and its map "+
			"entry stayed, which is a per-instrument leak keyed off the wire", series)
	}
}

// A CANDLE ALREADY PAST THE HORIZON IS NOT ADMITTED.
//
// A replay delivers oldest-first, so without this the whole retained history
// would be inserted and then swept message by message — the same end state,
// reached by allocating every candle the stream holds.
func TestFold_ACandlePastTheHorizonIsNotAdmitted(t *testing.T) {
	f := NewFold(2 * time.Minute)
	fold(t, f, candleAt("BTC-USD", "XSIM", tapeT0.Add(time.Hour), 10))
	fold(t, f, candleAt("BTC-USD", "XSIM", tapeT0, 10)) // an hour stale

	folded, _, evicted, _ := f.Stats()
	if folded != 1 || evicted != 1 {
		t.Errorf("folded=%d evicted=%d, want 1 and 1 — a stale candle must be counted out rather "+
			"than allocated and swept", folded, evicted)
	}
}

// EVERY UNREADABLE MESSAGE IS REFUSED, COUNTED AND ACKED.
//
// Counted, because a fold that has received nothing and one that has rejected
// everything both answer "unobservable" to every question — and only the refusal
// count separates a quiet feed from a producer this build cannot read.
func TestFold_UnreadableMessagesAreRefusedAndCounted(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"undecodable", []byte{0xff, 0xff, 0xff, 0xff}},
		{"no instrument", candleAt("", "XSIM", tapeT0, 10)},
		{"no venue", candleAt("BTC-USD", "", tapeT0, 10)},
		{"no open time", candleWith("BTC-USD", "XSIM", time.Time{}, tapeT0.Add(time.Minute),
			&commonpb.Decimal{Coefficient: 10})},
		{"close before open", candleWith("BTC-USD", "XSIM", tapeT0.Add(time.Minute), tapeT0,
			&commonpb.Decimal{Coefficient: 10})},
		{"no volume", candleWith("BTC-USD", "XSIM", tapeT0, tapeT0.Add(time.Minute), nil)},
		// THE EXPONENT DOMAIN (#95). Decimal.exponent is an unvalidated wire
		// field and dec.FromProto materialises 10^abs(exponent), so this message
		// would not produce a wrong denominator — the handler would never return,
		// and the subscription would stall while the pod reported healthy.
		{"out-of-domain exponent", candleWith("BTC-USD", "XSIM", tapeT0, tapeT0.Add(time.Minute),
			&commonpb.Decimal{Coefficient: 1, Exponent: 2000000000})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := NewFold(0)
			done := make(chan struct{})
			go func() {
				defer close(done)
				fold(t, f, c.payload)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Handle did not return — the subscription is stalled behind one message " +
					"and the service still reports healthy")
			}
			folded, refused, _, _ := f.Stats()
			if refused != 1 || folded != 0 {
				t.Errorf("folded=%d refused=%d, want 0 and 1", folded, refused)
			}
		})
	}
}

// A DEGENERATE QUERY IS UNKNOWN RATHER THAN ZERO, for the reason every other
// absence here is: the caller must count it as coverage, and a zero would be a
// denominator.
func TestVolume_ADegenerateQueryIsUnknown(t *testing.T) {
	f := NewFold(0)
	fold(t, f, candleAt("BTC-USD", "XSIM", tapeT0, 10))

	for _, c := range []struct {
		name       string
		inst, venu string
		from, to   time.Time
	}{
		{"no instrument", "", "XSIM", tapeT0, tapeT0.Add(time.Minute)},
		{"no venue", "BTC-USD", "", tapeT0, tapeT0.Add(time.Minute)},
		{"empty window", "BTC-USD", "XSIM", tapeT0, tapeT0},
		{"reversed window", "BTC-USD", "XSIM", tapeT0.Add(time.Minute), tapeT0},
	} {
		if v, ok := f.Volume(c.inst, c.venu, c.from, c.to); ok {
			t.Errorf("%s answered %s, want UNKNOWN", c.name, v.RatString())
		}
	}
}

package feed

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
)

// --- shared driver lifecycle (generic over a synthetic native tick) ---

type fakeTick struct {
	sym string
	px  float64
	bad bool
}

func fakeDecode(t fakeTick) (RawTick, error) {
	if t.bad {
		return RawTick{}, errors.New("decode boom")
	}
	return RawTick{
		VendorSymbol: t.sym, MIC: "XNAS", EventTime: ts(1), SourceSequence: 1,
		Kind: KindTrade, Price: DecimalFromFloat(t.px, priceExp), Size: DecimalFromFloat(1, 0),
	}, nil
}

// streamThenIdle returns a connect that (after errFirst failures) streams ticks
// then keeps the channel open until ctx — so the driver does not reconnect mid-
// test; a cancel-on-complete sink stops it.
func streamThenIdle(ticks []fakeTick, errFirst int) func(context.Context, []string) (<-chan fakeTick, error) {
	attempts := 0
	return func(ctx context.Context, _ []string) (<-chan fakeTick, error) {
		if attempts < errFirst {
			attempts++
			return nil, errors.New("connect failed")
		}
		ch := make(chan fakeTick)
		go func() {
			defer close(ch)
			for _, t := range ticks {
				select {
				case ch <- t:
				case <-ctx.Done():
					return
				}
			}
			<-ctx.Done()
		}()
		return ch, nil
	}
}

func testDriver(connect func(context.Context, []string) (<-chan fakeTick, error), xwalk Crosswalk) *Driver[fakeTick] {
	return &Driver[fakeTick]{
		vendor: "TEST", scheme: "TEST", connect: connect, decode: fakeDecode,
		xwalk: xwalk, base: time.Millisecond, max: 4 * time.Millisecond,
	}
}

type cancelSink struct {
	mu     sync.Mutex
	evs    []*marketpb.MarketDataEvent
	want   int
	cancel context.CancelFunc
	err    error
}

func (s *cancelSink) Publish(_ context.Context, ev *marketpb.MarketDataEvent) error {
	if s.err != nil {
		return s.err
	}
	s.mu.Lock()
	s.evs = append(s.evs, ev)
	n := len(s.evs)
	s.mu.Unlock()
	if n >= s.want && s.cancel != nil {
		s.cancel()
	}
	return nil
}

func (s *cancelSink) events() []*marketpb.MarketDataEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*marketpb.MarketDataEvent, len(s.evs))
	copy(out, s.evs)
	return out
}

func runUntil(t *testing.T, a Adapter, instruments []string, sink *cancelSink) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sink.cancel = cancel
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, instruments, sink) }()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
		return nil
	}
}

func staticXwalk(scheme IDScheme, pairs ...[2]string) *StaticCrosswalk {
	x := NewStaticCrosswalk()
	for _, p := range pairs {
		x.Add(scheme, p[0], p[1])
	}
	return x
}

func TestDriver_DecodeCrosswalkPublish(t *testing.T) {
	xwalk := staticXwalk("TEST", [2]string{"VOD.L", "VOD"})
	d := testDriver(streamThenIdle([]fakeTick{{sym: "VOD.L", px: 12.34}, {sym: "VOD.L", px: 12.35}}, 0), xwalk)
	sink := &cancelSink{want: 2}
	if err := runUntil(t, d, []string{"VOD"}, sink); err != nil {
		t.Fatal(err)
	}
	evs := sink.events()
	if len(evs) < 2 {
		t.Fatalf("got %d events, want 2", len(evs))
	}
	if evs[0].GetInstrumentId() != "VOD" { // resolved from the vendor symbol VOD.L
		t.Errorf("instrument_id = %q, want VOD (crosswalk)", evs[0].GetInstrumentId())
	}
	if err := Validate(evs[0]); err != nil {
		t.Errorf("normalized event invalid: %v", err)
	}
}

func TestDriver_DropsUnmappedAndUndecodable(t *testing.T) {
	xwalk := staticXwalk("TEST", [2]string{"VOD.L", "VOD"})
	// One good, one unmapped symbol, one undecodable — then a trailing good so the
	// sink reaches want=2 deterministically after the drops.
	ticks := []fakeTick{
		{sym: "VOD.L", px: 1},
		{sym: "NOPE.L", px: 2}, // not in the crosswalk → dropped (unmapped)
		{sym: "VOD.L", bad: true},
		{sym: "VOD.L", px: 3},
	}
	d := testDriver(streamThenIdle(ticks, 0), xwalk)
	var drops []string
	d.OnDrop = func(reason, _ string) { drops = append(drops, reason) }
	sink := &cancelSink{want: 2}
	if err := runUntil(t, d, []string{"VOD"}, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.events()) != 2 {
		t.Fatalf("published %d, want 2 (the two good ticks)", len(sink.events()))
	}
	gotUnmapped, gotDecode := false, false
	for _, r := range drops {
		switch r {
		case "unmapped":
			gotUnmapped = true
		case "decode":
			gotDecode = true
		}
	}
	if !gotUnmapped || !gotDecode {
		t.Errorf("drops = %v, want both unmapped + decode", drops)
	}
}

func TestDriver_ReconnectsThenStreams(t *testing.T) {
	xwalk := staticXwalk("TEST", [2]string{"VOD.L", "VOD"})
	// First two connects fail; the driver backs off and retries until it streams.
	d := testDriver(streamThenIdle([]fakeTick{{sym: "VOD.L", px: 9}}, 2), xwalk)
	sink := &cancelSink{want: 1}
	if err := runUntil(t, d, []string{"VOD"}, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.events()) != 1 {
		t.Fatalf("expected 1 event after reconnect, got %d", len(sink.events()))
	}
}

func TestDriver_SinkErrorStops(t *testing.T) {
	xwalk := staticXwalk("TEST", [2]string{"VOD.L", "VOD"})
	d := testDriver(streamThenIdle([]fakeTick{{sym: "VOD.L", px: 1}}, 0), xwalk)
	boom := errors.New("publish failed")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := d.Run(ctx, []string{"VOD"}, SinkFunc(func(context.Context, *marketpb.MarketDataEvent) error { return boom }))
	if !errors.Is(err, boom) {
		t.Fatalf("Run err = %v, want boom (a sink error is fatal)", err)
	}
}

func TestDriver_ContextCancelStops(t *testing.T) {
	xwalk := staticXwalk("TEST")
	d := testDriver(streamThenIdle(nil, 0), xwalk)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Run(ctx, nil, SinkFunc(func(context.Context, *marketpb.MarketDataEvent) error { return nil })); err != nil {
		t.Errorf("canceled Run = %v, want nil", err)
	}
}

func TestDriver_UnsubscribableInstrumentDropped(t *testing.T) {
	// A requested instrument with no vendor symbol is dropped at subscribe time.
	d := testDriver(streamThenIdle(nil, 0), NewStaticCrosswalk())
	var reasons []string
	d.OnDrop = func(reason, _ string) { reasons = append(reasons, reason) }
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = d.Run(ctx, []string{"UNKNOWN"}, SinkFunc(func(context.Context, *marketpb.MarketDataEvent) error { return nil }))
	if len(reasons) != 1 || reasons[0] != "no_vendor_symbol" {
		t.Errorf("reasons = %v, want [no_vendor_symbol]", reasons)
	}
}

// --- per-vendor decoders ---

func TestDecodeBloomberg(t *testing.T) {
	tr, err := decodeBloomberg(BloombergTick{Security: "AAPL US Equity", MIC: "XNAS", Type: "TRADE", Time: ts(1), Seq: 7, LastPrice: 150.05, LastSize: 100, TradeID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Kind != KindTrade || tr.VendorSymbol != "AAPL US Equity" || tr.SourceSequence != 7 {
		t.Errorf("trade decode = %+v", tr)
	}
	if tr.Price.GetCoefficient() != 1500500 || tr.Price.GetExponent() != -4 {
		t.Errorf("price = %dx10^%d, want 1500500x10^-4", tr.Price.GetCoefficient(), tr.Price.GetExponent())
	}
	q, err := decodeBloomberg(BloombergTick{Security: "AAPL US Equity", MIC: "XNAS", Type: "QUOTE", Time: ts(2), Bid: 150.00, Ask: 150.10, BidSize: 2, AskSize: 3})
	if err != nil || q.Kind != KindQuote || q.AskPrice.GetCoefficient() != 1501000 {
		t.Errorf("quote decode = %+v err=%v", q, err)
	}
	if _, err := decodeBloomberg(BloombergTick{Type: "FOO"}); err == nil {
		t.Error("unknown event type should error")
	}
}

func TestDecodeRefinitivAndICE(t *testing.T) {
	r, err := decodeRefinitiv(RefinitivTick{RIC: "AAPL.O", MIC: "XNAS", Update: "TRADE", Time: ts(1), Seq: 1, TrdPrice: 99.99, TrdVol: 50})
	if err != nil || r.Kind != KindTrade || r.VendorSymbol != "AAPL.O" || r.Price.GetCoefficient() != 999900 {
		t.Errorf("refinitiv trade = %+v err=%v", r, err)
	}
	i, err := decodeICE(ICETick{Symbol: "BRN", MIC: "IFEU", MsgType: "Q", Time: ts(1), SeqNum: 3, BidPx: 80.01, AskPx: 80.05, BidSz: 10, AskSz: 12})
	if err != nil || i.Kind != KindQuote || i.VendorSymbol != "BRN" || i.BidPrice.GetCoefficient() != 800100 {
		t.Errorf("ice quote = %+v err=%v", i, err)
	}
	if _, err := decodeICE(ICETick{MsgType: "Z"}); err == nil {
		t.Error("unknown ICE message type should error")
	}
}

// --- per-vendor end-to-end (decode + crosswalk + normalize + lifecycle) ---

type fakeBloomberg struct{ ticks []BloombergTick }

func (f fakeBloomberg) Subscribe(ctx context.Context, _ []string) (<-chan BloombergTick, error) {
	ch := make(chan BloombergTick)
	go func() {
		defer close(ch)
		for _, t := range f.ticks {
			select {
			case ch <- t:
			case <-ctx.Done():
				return
			}
		}
		<-ctx.Done()
	}()
	return ch, nil
}

func TestBloombergAdapter_EndToEnd(t *testing.T) {
	xwalk := staticXwalk(SchemeBloomberg, [2]string{"AAPL US Equity", "AAPL"})
	src := fakeBloomberg{ticks: []BloombergTick{
		{Security: "AAPL US Equity", MIC: "XNAS", Type: "TRADE", Time: ts(1), Seq: 1, LastPrice: 150.05, LastSize: 100, TradeID: "t1"},
		{Security: "AAPL US Equity", MIC: "XNAS", Type: "QUOTE", Time: ts(2), Seq: 2, Bid: 150, Ask: 150.10, BidSize: 2, AskSize: 3},
	}}
	a := NewBloomberg(src, xwalk)
	if a.Vendor() != "BLOOMBERG" {
		t.Errorf("vendor = %q", a.Vendor())
	}
	sink := &cancelSink{want: 2}
	if err := runUntil(t, a, []string{"AAPL"}, sink); err != nil {
		t.Fatal(err)
	}
	evs := sink.events()
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2", len(evs))
	}
	for _, ev := range evs {
		if ev.GetInstrumentId() != "AAPL" {
			t.Errorf("instrument_id = %q, want AAPL", ev.GetInstrumentId())
		}
		if err := Validate(ev); err != nil {
			t.Errorf("invalid normalized event: %v", err)
		}
	}
	if g := OutOfOrder(evs); len(g) != 0 {
		t.Errorf("ordering: %v", g)
	}
	if _, ok := evs[0].GetData().(*marketpb.MarketDataEvent_Trade); !ok {
		t.Error("first event should be a trade")
	}
	if _, ok := evs[1].GetData().(*marketpb.MarketDataEvent_Quote); !ok {
		t.Error("second event should be a quote")
	}
}

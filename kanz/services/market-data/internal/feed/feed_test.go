package feed

import (
	"context"
	"errors"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func dec(coeff int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
}

func ts(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

func meta(id string, sec int64, seq uint64) Meta {
	return Meta{InstrumentID: id, Symbol: id, MIC: "XNAS", EventTime: ts(sec), SourceSequence: seq}
}

func TestBuilders_ValidateRequiredFields(t *testing.T) {
	// A valid trade builds and passes Validate.
	tr, err := Trade(meta("AAPL", 1, 1), dec(1500000, -4), dec(100, 0), "t1")
	if err != nil {
		t.Fatalf("valid trade: %v", err)
	}
	if err := Validate(tr); err != nil {
		t.Fatalf("built trade fails Validate: %v", err)
	}
	// Missing price ⇒ rejected at the edge.
	if _, err := Trade(meta("AAPL", 1, 1), nil, dec(100, 0), "t1"); err != ErrBadDecimal {
		t.Errorf("trade without price: err = %v, want ErrBadDecimal", err)
	}
	// Missing envelope fields ⇒ rejected.
	if _, err := Trade(Meta{Symbol: "AAPL", MIC: "XNAS", EventTime: ts(1)}, dec(1, 0), dec(1, 0), ""); err != ErrNoInstrument {
		t.Errorf("no instrument: err = %v, want ErrNoInstrument", err)
	}
	if _, err := Quote(meta("AAPL", 1, 1), dec(1, 0), dec(1, 0), dec(1, 0), nil); err != ErrBadDecimal {
		t.Errorf("quote missing ask size: err = %v, want ErrBadDecimal", err)
	}
	bar, err := Bar(meta("AAPL", 60, 1), dec(1, 0), dec(2, 0), dec(1, 0), dec(2, 0), dec(1000, 0), ts(0), 42)
	if err != nil || Validate(bar) != nil {
		t.Fatalf("valid bar: err=%v validate=%v", err, Validate(bar))
	}
}

func TestValidate_RejectsMalformed(t *testing.T) {
	// An event with no data variant is invalid.
	bad := &marketpb.MarketDataEvent{InstrumentId: "AAPL", Symbol: "AAPL", Mic: "XNAS", EventTime: timestamppb.New(ts(1))}
	if Validate(bad) != ErrNoData {
		t.Errorf("no-data event: want ErrNoData, got %v", Validate(bad))
	}
	if Validate(nil) != ErrNoData {
		t.Errorf("nil event: want ErrNoData")
	}
}

func TestDecimalFromFloat(t *testing.T) {
	d := DecimalFromFloat(150.05, -4)
	if d.GetCoefficient() != 1500500 || d.GetExponent() != -4 {
		t.Errorf("150.05@-4 = %dx10^%d, want 1500500x10^-4", d.GetCoefficient(), d.GetExponent())
	}
}

// goldenSession is a small two-instrument session used by the Sim + Conform tests.
func goldenSession(t *testing.T) Session {
	t.Helper()
	mk := func(id string, sec int64, seq uint64) *marketpb.MarketDataEvent {
		ev, err := Trade(meta(id, sec, seq), dec(int64(sec), -2), dec(10, 0), "")
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	return Session{
		mk("AAPL", 1, 1), mk("MSFT", 1, 1), mk("AAPL", 2, 2), mk("MSFT", 3, 2), mk("AAPL", 4, 3),
	}
}

func TestSimAdapter_ReplaysAndFilters(t *testing.T) {
	sim := &SimAdapter{Name: "BLOOMBERG-SIM", Session: goldenSession(t)}
	if sim.Vendor() != "BLOOMBERG-SIM" {
		t.Errorf("vendor = %q", sim.Vendor())
	}
	var got []*marketpb.MarketDataEvent
	err := sim.Run(context.Background(), []string{"AAPL"}, SinkFunc(func(_ context.Context, ev *marketpb.MarketDataEvent) error {
		got = append(got, ev)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	// Only the 3 AAPL events are delivered (MSFT filtered out).
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3 AAPL", len(got))
	}
	for _, ev := range got {
		if ev.GetInstrumentId() != "AAPL" {
			t.Errorf("delivered unsubscribed %q", ev.GetInstrumentId())
		}
	}
}

func TestSimAdapter_SinkErrorAborts(t *testing.T) {
	sim := &SimAdapter{Session: goldenSession(t)}
	boom := errors.New("backpressure")
	n := 0
	err := sim.Run(context.Background(), nil, SinkFunc(func(_ context.Context, _ *marketpb.MarketDataEvent) error {
		n++
		return boom // a slow/failing sink backpressures and aborts the run
	}))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if n != 1 {
		t.Errorf("published %d before abort, want 1 (no buffering past the sink)", n)
	}
}

func TestSimAdapter_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (&SimAdapter{Session: goldenSession(t), Loop: true}).Run(ctx, nil, SinkFunc(func(context.Context, *marketpb.MarketDataEvent) error { return nil }))
	if err != nil {
		t.Errorf("canceled run should return nil, got %v", err)
	}
}

func TestConform_PassesForSim(t *testing.T) {
	// The Sim adapter, fed its own golden session as the expectation, conforms.
	want := goldenSession(t)
	sim := &SimAdapter{Session: want}
	if v := Conform(sim, []string{"AAPL", "MSFT"}, want); len(v) != 0 {
		t.Fatalf("Sim should conform, violations: %v", v)
	}
}

// badAdapter emits an out-of-order, sequence-gapped, partly-invalid stream — the
// harness must catch every defect.
type badAdapter struct{ evs Session }

func (b badAdapter) Vendor() string { return "BAD" }
func (b badAdapter) Run(ctx context.Context, _ []string, sink Sink) error {
	for _, ev := range b.evs {
		if err := sink.Publish(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

func TestConform_CatchesDefects(t *testing.T) {
	mk := func(id string, sec int64, seq uint64) *marketpb.MarketDataEvent {
		ev, _ := Trade(meta(id, sec, seq), dec(1, 0), dec(1, 0), "")
		return ev
	}
	invalid := &marketpb.MarketDataEvent{InstrumentId: "AAPL", Symbol: "AAPL", Mic: "XNAS", EventTime: timestamppb.New(ts(9))} // no data
	bad := badAdapter{evs: Session{
		mk("AAPL", 5, 1),
		mk("AAPL", 2, 2), // event_time goes backwards ⇒ OutOfOrder
		mk("AAPL", 6, 5), // sequence jumps 2→5 ⇒ Gap
		invalid,          // fails Validate
	}}
	v := Conform(bad, []string{"AAPL"}, bad.evs)
	if len(v) == 0 {
		t.Fatal("expected violations for the bad adapter")
	}
	joined := ""
	for _, s := range v {
		joined += s + "\n"
	}
	for _, want := range []string{"precedes", "expected 3", "invalid"} {
		if !contains(joined, want) {
			t.Errorf("violations missing %q; got:\n%s", want, joined)
		}
	}
}

func TestConform_FlagsUnsubscribed(t *testing.T) {
	want := goldenSession(t)
	// Subscribe only to AAPL but the Sim is given the full session as want —
	// the harness flags the MSFT events as unsubscribed + the count mismatch.
	v := Conform(&SimAdapter{Session: want}, []string{"AAPL"}, want)
	if len(v) == 0 {
		t.Fatal("expected violations: MSFT delivered? no — filtered, so count mismatch vs want")
	}
}

func TestGapAndOutOfOrder(t *testing.T) {
	mk := func(id string, sec int64, seq uint64) *marketpb.MarketDataEvent {
		ev, _ := Trade(meta(id, sec, seq), dec(1, 0), dec(1, 0), "")
		return ev
	}
	clean := []*marketpb.MarketDataEvent{mk("A", 1, 1), mk("B", 1, 1), mk("A", 2, 2)}
	if g := Gap(clean); len(g) != 0 {
		t.Errorf("clean Gap: %v", g)
	}
	if o := OutOfOrder(clean); len(o) != 0 {
		t.Errorf("clean OutOfOrder: %v", o)
	}
	if g := Gap([]*marketpb.MarketDataEvent{mk("A", 1, 1), mk("A", 2, 4)}); len(g) != 1 {
		t.Errorf("gap not detected: %v", g)
	}
	// seq 0 (no venue sequence) is skipped, not flagged.
	if g := Gap([]*marketpb.MarketDataEvent{mk("A", 1, 0), mk("A", 2, 0)}); len(g) != 0 {
		t.Errorf("seq-0 should be skipped: %v", g)
	}
}

func TestReconnect_RetriesThenSucceeds(t *testing.T) {
	calls := 0
	err := Reconnect(context.Background(), time.Millisecond, 5*time.Millisecond, func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("transient disconnect")
		}
		return nil // clean shutdown on the 3rd attempt
	})
	if err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestReconnect_StopsOnContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Reconnect(ctx, time.Millisecond, time.Millisecond, func(context.Context) error {
		return errors.New("transient")
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

package livequote

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/risk/pricing/curve"
)

func decv(coeff int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
}

func quoteEvent(id string, bid, ask *commonpb.Decimal) *marketpb.MarketDataEvent {
	return &marketpb.MarketDataEvent{
		InstrumentId: id,
		Data:         &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{BidPrice: bid, AskPrice: ask}},
	}
}

// Handler decodes a market.v1 payload off the bus and folds it into the cache,
// so a subsequent Latest sees it — the risk-engine subscribe path.
func TestHandlerFoldsIntoCache(t *testing.T) {
	q := New()
	ev := quoteEvent("USD-DEP-3M", decv(44, -3), decv(46, -3))
	payload, err := proto.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	env := &envelopepb.Envelope{EventType: "market.rate.quote"}
	if err := q.Handler(context.Background(), env, payload); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if _, ok := q.Latest("USD-DEP-3M"); !ok {
		t.Fatal("cache did not record the folded event")
	}
}

func TestHandlerRejectsMalformedPayload(t *testing.T) {
	q := New()
	env := &envelopepb.Envelope{EventType: "market.rate.quote"}
	if err := q.Handler(context.Background(), env, []byte("not a proto")); err == nil {
		t.Fatal("expected a decode error for a malformed payload")
	}
}

// The adapter emits one RateQuote per configured instrument with a value,
// carrying the reference role and the cached mid.
func TestSnapshotRateSourceEmitsStrip(t *testing.T) {
	q := New()
	q.Update(quoteEvent("USD-DEP-3M", decv(44, -3), decv(46, -3))) // mid 0.045
	q.Update(quoteEvent("USD-SWAP-5Y", decv(40, -3), decv(40, -3)))

	src := NewSnapshotRateSource(q, []RateInstrument{
		{InstrumentID: "USD-DEP-3M", Currency: "USD", Kind: curve.Deposit, Tenor: 0.25},
		{InstrumentID: "USD-SWAP-5Y", Currency: "USD", Kind: curve.Swap, Tenor: 5},
	})
	got, err := src.RateQuotes(context.Background(), "USD", time.Now())
	if err != nil {
		t.Fatalf("RateQuotes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d quotes, want 2: %+v", len(got), got)
	}
	if got[0].Kind != curve.Deposit || !approx(got[0].Value, 0.045) {
		t.Fatalf("deposit quote wrong: %+v", got[0])
	}
}

// Missing ticks are skipped; other-currency instruments are filtered.
func TestSnapshotRateSourceSkipsAndFilters(t *testing.T) {
	q := New()
	q.Update(quoteEvent("USD-DEP-3M", decv(44, -3), decv(46, -3)))

	src := NewSnapshotRateSource(q, []RateInstrument{
		{InstrumentID: "USD-DEP-3M", Currency: "USD", Kind: curve.Deposit, Tenor: 0.25},
		{InstrumentID: "USD-SWAP-5Y", Currency: "USD", Kind: curve.Swap, Tenor: 5}, // no tick
		{InstrumentID: "EUR-DEP-3M", Currency: "EUR", Kind: curve.Deposit, Tenor: 0.25},
	})
	got, err := src.RateQuotes(context.Background(), "USD", time.Now())
	if err != nil {
		t.Fatalf("RateQuotes: %v", err)
	}
	if len(got) != 1 || got[0].Kind != curve.Deposit {
		t.Fatalf("want only the ticked USD deposit, got %+v", got)
	}
}

func TestCurrencies(t *testing.T) {
	src := NewSnapshotRateSource(New(), []RateInstrument{
		{InstrumentID: "USD-A", Currency: "USD"},
		{InstrumentID: "EUR-A", Currency: "EUR"},
		{InstrumentID: "USD-B", Currency: "USD"},
	})
	got := src.Currencies()
	if len(got) != 2 || got[0] != "USD" || got[1] != "EUR" {
		t.Fatalf("Currencies = %v, want [USD EUR]", got)
	}
}

// End-to-end: a live strip calibrates a curve through the real curve.Calibrator
// and publishes a usable discount factor.
func TestCalibratesUsableCurve(t *testing.T) {
	q := New()
	q.Update(quoteEvent("USD-DEP-1Y", decv(40, -3), decv(40, -3)))
	q.Update(quoteEvent("USD-SWAP-2Y", decv(42, -3), decv(42, -3)))
	q.Update(quoteEvent("USD-SWAP-5Y", decv(45, -3), decv(45, -3)))

	src := NewSnapshotRateSource(q, []RateInstrument{
		{InstrumentID: "USD-DEP-1Y", Currency: "USD", Kind: curve.Deposit, Tenor: 1},
		{InstrumentID: "USD-SWAP-2Y", Currency: "USD", Kind: curve.Swap, Tenor: 2},
		{InstrumentID: "USD-SWAP-5Y", Currency: "USD", Kind: curve.Swap, Tenor: 5},
	})
	cal := &curve.Calibrator{Source: src, Store: curve.NewStore(), Interp: curve.LinearZero}
	asOf := time.Now()
	if _, err := cal.Refresh(context.Background(), "USD", asOf); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	c, ok := cal.Store.Curve(context.Background(), "USD", asOf)
	if !ok {
		t.Fatal("no curve published")
	}
	if df := c.Discount(1); df <= 0 || df >= 1 {
		t.Fatalf("1Y discount factor out of range: %v", df)
	}
}

// An empty strip leaves the store unchanged (deny-on-garbage) — the inert-but-
// correct behavior when no rate instruments have ticked.
func TestEmptyStripDenyOnGarbage(t *testing.T) {
	src := NewSnapshotRateSource(New(), []RateInstrument{
		{InstrumentID: "USD-DEP-3M", Currency: "USD", Kind: curve.Deposit, Tenor: 0.25},
	})
	store := curve.NewStore()
	cal := &curve.Calibrator{Source: src, Store: store, Interp: curve.LinearZero}
	if _, err := cal.Refresh(context.Background(), "USD", time.Now()); err == nil {
		t.Fatal("expected Refresh to reject an empty strip")
	}
	if _, ok := store.Curve(context.Background(), "USD", time.Now()); ok {
		t.Fatal("store should stay empty after a failed calibration")
	}
}

func TestParseRateInstruments(t *testing.T) {
	got, err := ParseRateInstruments("USD-DEP-3M:USD:deposit:0.25, USD-FUT-1Y:USD:future:1:0.25 ,USD-SWAP-5Y:USD:swap:5")
	if err != nil {
		t.Fatalf("ParseRateInstruments: %v", err)
	}
	want := []RateInstrument{
		{InstrumentID: "USD-DEP-3M", Currency: "USD", Kind: curve.Deposit, Tenor: 0.25},
		{InstrumentID: "USD-FUT-1Y", Currency: "USD", Kind: curve.Future, Tenor: 1, Span: 0.25},
		{InstrumentID: "USD-SWAP-5Y", Currency: "USD", Kind: curve.Swap, Tenor: 5},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("instrument %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseRateInstrumentsRejectsMalformed(t *testing.T) {
	for name, spec := range map[string]string{
		"too few":        "USD-DEP:USD:deposit",
		"too many":       "USD-DEP:USD:deposit:0.25:0.5:x",
		"unknown kind":   "USD-DEP:USD:bond:0.25",
		"bad tenor":      "USD-DEP:USD:deposit:soon",
		"zero tenor":     "USD-DEP:USD:deposit:0",
		"negative span":  "USD-FUT:USD:future:1:-0.25",
		"empty currency": "USD-DEP::deposit:0.25",
		"empty instr":    ":USD:deposit:0.25",
	} {
		if _, err := ParseRateInstruments(spec); err == nil {
			t.Errorf("%s: expected error for %q", name, spec)
		}
	}
}

func approx(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

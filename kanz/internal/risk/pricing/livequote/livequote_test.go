package livequote

import (
	"context"
	"fmt"
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
	q := New([]RateInstrument{{InstrumentID: "USD-DEP-3M", Currency: "USD", Kind: curve.Deposit, Tenor: 0.25}})
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
	q := New([]RateInstrument{{InstrumentID: "USD-DEP-3M", Currency: "USD", Kind: curve.Deposit, Tenor: 0.25}})
	env := &envelopepb.Envelope{EventType: "market.rate.quote"}
	if err := q.Handler(context.Background(), env, []byte("not a proto")); err == nil {
		t.Fatal("expected a decode error for a malformed payload")
	}
}

// The adapter emits one RateQuote per configured instrument with a value,
// carrying the reference role and the cached mid.
func TestSnapshotRateSourceEmitsStrip(t *testing.T) {
	instruments := []RateInstrument{
		{InstrumentID: "USD-DEP-3M", Currency: "USD", Kind: curve.Deposit, Tenor: 0.25},
		{InstrumentID: "USD-SWAP-5Y", Currency: "USD", Kind: curve.Swap, Tenor: 5},
	}
	q := New(instruments)
	q.Update(quoteEvent("USD-DEP-3M", decv(44, -3), decv(46, -3))) // mid 0.045
	q.Update(quoteEvent("USD-SWAP-5Y", decv(40, -3), decv(40, -3)))

	src := NewSnapshotRateSource(q, instruments)
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
	instruments := []RateInstrument{
		{InstrumentID: "USD-DEP-3M", Currency: "USD", Kind: curve.Deposit, Tenor: 0.25},
		{InstrumentID: "USD-SWAP-5Y", Currency: "USD", Kind: curve.Swap, Tenor: 5}, // no tick
		{InstrumentID: "EUR-DEP-3M", Currency: "EUR", Kind: curve.Deposit, Tenor: 0.25},
	}
	q := New(instruments)
	q.Update(quoteEvent("USD-DEP-3M", decv(44, -3), decv(46, -3)))

	src := NewSnapshotRateSource(q, instruments)
	got, err := src.RateQuotes(context.Background(), "USD", time.Now())
	if err != nil {
		t.Fatalf("RateQuotes: %v", err)
	}
	if len(got) != 1 || got[0].Kind != curve.Deposit {
		t.Fatalf("want only the ticked USD deposit, got %+v", got)
	}
}

func TestCurrencies(t *testing.T) {
	instruments := []RateInstrument{
		{InstrumentID: "USD-A", Currency: "USD"},
		{InstrumentID: "EUR-A", Currency: "EUR"},
		{InstrumentID: "USD-B", Currency: "USD"},
	}
	src := NewSnapshotRateSource(New(instruments), instruments)
	got := src.Currencies()
	if len(got) != 2 || got[0] != "USD" || got[1] != "EUR" {
		t.Fatalf("Currencies = %v, want [USD EUR]", got)
	}
}

// End-to-end: a live strip calibrates a curve through the real curve.Calibrator
// and publishes a usable discount factor.
func TestCalibratesUsableCurve(t *testing.T) {
	instruments := []RateInstrument{
		{InstrumentID: "USD-DEP-1Y", Currency: "USD", Kind: curve.Deposit, Tenor: 1},
		{InstrumentID: "USD-SWAP-2Y", Currency: "USD", Kind: curve.Swap, Tenor: 2},
		{InstrumentID: "USD-SWAP-5Y", Currency: "USD", Kind: curve.Swap, Tenor: 5},
	}
	q := New(instruments)
	q.Update(quoteEvent("USD-DEP-1Y", decv(40, -3), decv(40, -3)))
	q.Update(quoteEvent("USD-SWAP-2Y", decv(42, -3), decv(42, -3)))
	q.Update(quoteEvent("USD-SWAP-5Y", decv(45, -3), decv(45, -3)))

	src := NewSnapshotRateSource(q, instruments)
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
	instruments := []RateInstrument{
		{InstrumentID: "USD-DEP-3M", Currency: "USD", Kind: curve.Deposit, Tenor: 0.25},
	}
	src := NewSnapshotRateSource(New(instruments), instruments)
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

// foldQuote drives ev through the real bus handler — the path the risk-engine
// subscribes — and returns the handler's verdict. Going through Handler rather
// than Update is the point: the wildcard subscription is what delivers the
// out-of-universe traffic these tests are about.
func foldQuote(t *testing.T, q *LiveQuotes, ev *marketpb.MarketDataEvent) error {
	t.Helper()
	payload, err := proto.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return q.Handler(context.Background(), &envelopepb.Envelope{EventType: "market.rate.quote"}, payload)
}

// THE BOUND, MEASURED (#894). cfg.MarketSubjects defaults to the `market.>`
// wildcard, so the cache is handed every instrument the spine carries. The map
// must hold the configured strip and nothing else, however much else arrives —
// and the strip must still calibrate, or the filter has bought a bound by
// breaking the reader.
func TestCacheAdmitsOnlyTheConfiguredUniverse(t *testing.T) {
	instruments := []RateInstrument{
		{InstrumentID: "USD-DEP-3M", Currency: "USD", Kind: curve.Deposit, Tenor: 0.25},
		{InstrumentID: "USD-SWAP-5Y", Currency: "USD", Kind: curve.Swap, Tenor: 5},
	}
	q := New(instruments)

	const offUniverse = 5000
	for i := range offUniverse {
		id := fmt.Sprintf("SPINE-INSTR-%06d", i)
		if err := foldQuote(t, q, quoteEvent(id, decv(1, 0), decv(2, 0))); err != nil {
			t.Fatalf("Handler nacked the out-of-universe %s: %v. On a market.> wildcard almost every "+
				"event is out of universe, so nacking them DLQs the market spine", id, err)
		}
	}
	for _, in := range instruments {
		if err := foldQuote(t, q, quoteEvent(in.InstrumentID, decv(44, -3), decv(46, -3))); err != nil {
			t.Fatalf("Handler rejected the configured %s: %v", in.InstrumentID, err)
		}
	}

	if got := len(q.latest); got != len(instruments) {
		t.Fatalf("the cache holds %d entries after %d out-of-universe events were folded through "+
			"Handler, want %d. LiveQuotes.latest is bounded by the CONFIGURED calibration strip, not "+
			"by what the market.> wildcard delivers — an entry per instrument the spine has ever "+
			"published is a leak for the life of the pod (#894)", got, offUniverse, len(instruments))
	}
	if _, ok := q.Latest("SPINE-INSTR-000000"); ok {
		t.Fatal("an out-of-universe instrument is readable from the cache, so it was retained")
	}
	for _, in := range instruments {
		if _, ok := q.Latest(in.InstrumentID); !ok {
			t.Fatalf("the configured %s is NOT in the cache — the filter dropped a quote the "+
				"calibrator needs", in.InstrumentID)
		}
	}

	src := NewSnapshotRateSource(q, instruments)
	got, err := src.RateQuotes(context.Background(), "USD", time.Now())
	if err != nil {
		t.Fatalf("RateQuotes: %v", err)
	}
	if len(got) != len(instruments) {
		t.Fatalf("the strip lost instruments to the admission filter: %+v", got)
	}
}

// Universe is the ADMITTED set, not the cached one: it answers "what does this
// pod retain quotes for" at startup, before anything has ticked. Returning the
// cached keys instead would make the composition root's log empty exactly when
// an operator reads it.
func TestUniverseIsTheAdmittedSet(t *testing.T) {
	q := New([]RateInstrument{
		{InstrumentID: "USD-SWAP-5Y", Currency: "USD", Kind: curve.Swap, Tenor: 5},
		{InstrumentID: "USD-DEP-3M", Currency: "USD", Kind: curve.Deposit, Tenor: 0.25},
	})
	got := q.Universe()
	if len(got) != 2 || got[0] != "USD-DEP-3M" || got[1] != "USD-SWAP-5Y" {
		t.Fatalf("Universe = %v, want the configured pair sorted before any tick arrives", got)
	}
}

// An unconfigured cache admits NOTHING rather than everything. The composition
// root refuses to start on an empty spec, so this is the type's own fail-closed
// posture rather than a reachable production state.
func TestEmptyUniverseAdmitsNothing(t *testing.T) {
	q := New(nil)
	if err := foldQuote(t, q, quoteEvent("USD-DEP-3M", decv(44, -3), decv(46, -3))); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if got := len(q.latest); got != 0 {
		t.Fatalf("an empty universe cached %d event(s) — an unconfigured cache must admit nothing, "+
			"not everything the wildcard carries", got)
	}
}

func approx(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

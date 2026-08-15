// THE PRODUCTION SpotProvider (#509).
//
// UNGATED — no TEST_POSTGRES_URL, no database, no skip. The provider depends on
// PriceStore, which is one method, so the bitemporal property that matters most
// is proven against store.Memory (the real in-memory implementation of the
// MODEL-01b contract, corrections and all) and the failure paths are proven
// against a three-field fake. A gated test that skips silently on every
// developer machine would leave this exactly as unproven as it was before it
// existed.
package spotsource

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/marketdata/store"
)

// fakeStore answers one canned result and records what it was asked for.
type fakeStore struct {
	obs store.Observation
	ok  bool
	err error

	calls    int
	gotKind  store.PriceKind
	gotAsOf  time.Time
	gotInstr string
}

func (f *fakeStore) LatestAsOf(_ context.Context, instrumentID string, kind store.PriceKind, asOf time.Time) (store.Observation, bool, error) {
	f.calls++
	f.gotInstr, f.gotKind, f.gotAsOf = instrumentID, kind, asOf
	return f.obs, f.ok, f.err
}

func dec(coefficient int64, exponent int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coefficient, Exponent: exponent}
}

var t0 = time.Date(2026, 1, 5, 21, 0, 0, 0, time.UTC) // a Monday close

// mark builds a fake holding one usable observation, ObservationTime `age`
// before t0.
func mark(price *commonpb.Decimal, age time.Duration) *fakeStore {
	return &fakeStore{
		obs: store.Observation{
			InstrumentID:    "AAPL",
			ObservationTime: t0.Add(-age),
			Price:           price,
			Kind:            store.PriceKindClose,
			KnowledgeTime:   t0.Add(-age),
		},
		ok: true,
	}
}

func mustProvider(t *testing.T, s PriceStore, opts ...Option) *Provider {
	t.Helper()
	p, err := FromStore(s, opts...)
	if err != nil {
		t.Fatalf("FromStore: %v", err)
	}
	return p
}

// recorder captures unresolved reports so a test can assert the degradation was
// made visible, not merely that it happened.
type recorder struct {
	instr  []string
	reason []string
	age    []time.Duration
}

func (r *recorder) observe() Option {
	return WithUnresolvedObserver(func(instrumentID, reason string, age time.Duration) {
		r.instr = append(r.instr, instrumentID)
		r.reason = append(r.reason, reason)
		r.age = append(r.age, age)
	})
}

func (r *recorder) only(t *testing.T, wantReason string) time.Duration {
	t.Helper()
	if len(r.reason) != 1 {
		t.Fatalf("observer fired %d times %v, want exactly once with %q — an unresolved spot that "+
			"nobody counts is the invisible degradation this hook exists for", len(r.reason), r.reason, wantReason)
	}
	if r.reason[0] != wantReason {
		t.Fatalf("reason = %q, want %q", r.reason[0], wantReason)
	}
	return r.age[0]
}

func TestAStoredPriceResolvesAndKeepsItsFraction(t *testing.T) {
	f := mark(dec(15025, -2), time.Hour) // 150.25
	got, ok := mustProvider(t, f).Spot(context.Background(), "AAPL", t0)
	if !ok {
		t.Fatal("ok = false for a fresh, positive, stored mark")
	}
	if math.Abs(got-150.25) > 1e-9 {
		t.Errorf("spot = %v, want 150.25 — the Decimal→float conversion dropped the fraction, which "+
			"a whole-number test fixture would not have caught", got)
	}
}

func TestTheConfiguredKindIsTheOnlyMarkRead(t *testing.T) {
	f := mark(dec(100, 0), time.Hour)
	if _, ok := mustProvider(t, f).Spot(context.Background(), "AAPL", t0); !ok {
		t.Fatal("ok = false")
	}
	if f.gotKind != store.PriceKindClose {
		t.Errorf("default kind read = %v, want PriceKindClose (%v) — the returns and volatility "+
			"planes read Close, and a spot from another series prices the Greeks off a different market",
			f.gotKind, store.PriceKindClose)
	}

	f2 := mark(dec(100, 0), time.Hour)
	if _, ok := mustProvider(t, f2, WithPriceKind(store.PriceKindSettlement)).Spot(context.Background(), "AAPL", t0); !ok {
		t.Fatal("ok = false")
	}
	if f2.gotKind != store.PriceKindSettlement {
		t.Errorf("configured kind read = %v, want PriceKindSettlement — WithPriceKind is the escape "+
			"hatch for a deployment whose ticks do not land as Close, and it must reach the store",
			f2.gotKind)
	}
}

func TestAnInstrumentWithNoObservationIsRefusedAndReported(t *testing.T) {
	var r recorder
	p := mustProvider(t, &fakeStore{ok: false}, r.observe())

	if got, ok := p.Spot(context.Background(), "AAPL", t0); ok || got != 0 {
		t.Fatalf("Spot = (%v, %v), want (0, false) for an instrument the store has never marked", got, ok)
	}
	if age := r.only(t, ReasonNoObservation); age != 0 {
		t.Errorf("age = %v for a missing mark, want 0 — age is meaningful only for ReasonStale", age)
	}
	if r.instr[0] != "AAPL" {
		t.Errorf("observed instrument = %q, want AAPL — a count with no instrument cannot be acted on", r.instr[0])
	}
}

func TestAStoreErrorIsRefusedRatherThanPanicking(t *testing.T) {
	var r recorder
	p := mustProvider(t, &fakeStore{err: errors.New("connection refused")}, r.observe())

	got, ok := p.Spot(context.Background(), "AAPL", t0)
	if ok || got != 0 {
		t.Fatalf("Spot = (%v, %v), want (0, false) — the seam has no error channel, so a failed "+
			"read must collapse to false and not take the risk engine down", got, ok)
	}
	r.only(t, ReasonStoreError)
}

// THE PROPERTY THAT MATTERS MOST (MODEL-01i). A late vendor correction restates a
// past ObservationTime under a NEWER KnowledgeTime; a valuation as of a time
// before that correction was known must not see it, and must see it once the
// valuation time passes it.
//
// Asserted against store.Memory rather than a fake, deliberately: a fake I write
// would implement whatever filter I believe the store implements, so a fake that
// drops corrections entirely would pass the "invisible" half of this test while
// proving nothing. Both directions are asserted for the same reason — a store
// that silently discarded restatements would satisfy the first assertion alone.
func TestACorrectionIsInvisibleUntilTheValuationTimePassesIt(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	obsTime := t0
	original := store.Observation{
		InstrumentID: "AAPL", ObservationTime: obsTime, Price: dec(10000, -2),
		Kind: store.PriceKindClose, KnowledgeTime: obsTime,
	}
	correction := store.Observation{
		InstrumentID: "AAPL", ObservationTime: obsTime, Price: dec(15500, -2),
		Kind: store.PriceKindClose, KnowledgeTime: obsTime.Add(48 * time.Hour),
	}
	if err := mem.Put(ctx, []store.Observation{original, correction}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	p := mustProvider(t, mem)

	before, ok := p.Spot(ctx, "AAPL", obsTime.Add(24*time.Hour))
	if !ok {
		t.Fatal("ok = false one day after the mark")
	}
	if math.Abs(before-100.00) > 1e-9 {
		t.Errorf("spot as of the day after the close = %v, want 100.00 — a correction stamped with a "+
			"KnowledgeTime AFTER the valuation time leaked backward, and every replayed risk number "+
			"is then computed off knowledge that did not exist yet", before)
	}

	after, ok := p.Spot(ctx, "AAPL", obsTime.Add(72*time.Hour))
	if !ok {
		t.Fatal("ok = false three days after the mark")
	}
	if math.Abs(after-155.00) > 1e-9 {
		t.Errorf("spot as of after the correction was known = %v, want 155.00 — the restatement never "+
			"became visible, so this provider would serve a superseded price forever (and the "+
			"assertion above would pass on a store that simply drops corrections)", after)
	}
}

func TestAMarkOlderThanTheMaxAgeIsRefusedAtTheBoundary(t *testing.T) {
	const bound = 7 * 24 * time.Hour

	atBound := mark(dec(100, 0), bound)
	if _, ok := mustProvider(t, atBound, WithMaxAge(bound)).Spot(context.Background(), "AAPL", t0); !ok {
		t.Error("a mark exactly at the bound was refused — the rule is `age > maxAge`, so a Tuesday " +
			"valuation off the Thursday close either side of a two-holiday weekend must still price")
	}

	var r recorder
	past := mark(dec(100, 0), bound+time.Nanosecond)
	got, ok := mustProvider(t, past, WithMaxAge(bound), r.observe()).Spot(context.Background(), "AAPL", t0)
	if ok || got != 0 {
		t.Fatalf("Spot = (%v, %v) one nanosecond past the bound, want (0, false) — a Greek computed "+
			"off a mark that old is confidently wrong, which is worse than absent", got, ok)
	}
	age := r.only(t, ReasonStale)
	if age != bound+time.Nanosecond {
		t.Errorf("reported age = %v, want %v — the operator raises the bound on the measured age, so "+
			"reporting the wrong one is reporting nothing", age, bound+time.Nanosecond)
	}
}

// STALENESS IS MEASURED AGAINST asOf, NOT THE WALL CLOCK. Both marks below are
// years old in real time; only the one that was old AT ITS VALUATION TIME is
// refused. A wall-clock rule would return no Greeks for any historical replay.
func TestStalenessIsMeasuredAtTheValuationTimeNotNow(t *testing.T) {
	old := time.Date(2019, 3, 1, 21, 0, 0, 0, time.UTC)
	f := &fakeStore{
		obs: store.Observation{
			InstrumentID: "AAPL", ObservationTime: old, Price: dec(100, 0),
			Kind: store.PriceKindClose, KnowledgeTime: old,
		},
		ok: true,
	}
	if _, ok := mustProvider(t, f).Spot(context.Background(), "AAPL", old.Add(24*time.Hour)); !ok {
		t.Error("a 2019 mark was refused for a 2019 valuation — staleness measured against the wall " +
			"clock makes every backtest and every replayed revaluation return no Greeks at all")
	}
	if _, ok := mustProvider(t, f).Spot(context.Background(), "AAPL", t0); ok {
		t.Error("a 2019 mark priced a 2026 valuation under the default bound")
	}
}

func TestTheUnboundedOptionAcceptsAMarkOfAnyAge(t *testing.T) {
	f := mark(dec(100, 0), 5*365*24*time.Hour)
	if _, ok := mustProvider(t, f, WithoutStalenessBound()).Spot(context.Background(), "AAPL", t0); !ok {
		t.Error("WithoutStalenessBound refused a five-year-old mark — the opt-out exists for backfills " +
			"and must actually opt out")
	}
	// Order-independent: the opt-out wins whichever way round the options arrive,
	// so a config cannot depend on the order a caller happened to list them.
	if _, ok := mustProvider(t, f, WithoutStalenessBound(), WithMaxAge(time.Hour)).Spot(context.Background(), "AAPL", t0); !ok {
		t.Error("WithMaxAge after WithoutStalenessBound re-imposed a bound")
	}
	if _, ok := mustProvider(t, f, WithMaxAge(time.Hour), WithoutStalenessBound()).Spot(context.Background(), "AAPL", t0); !ok {
		t.Error("WithoutStalenessBound after WithMaxAge did not win")
	}
}

// greeks.go rejects spot <= 0 too. This rejects it EARLIER so the reason is
// distinguishable — upstream it is anonymous inside SkipNoSpot — and so the seam
// never answers ok=true with a price nothing can be priced from.
func TestAnUnusablePriceIsRefusedBeforeItReachesTheGreeks(t *testing.T) {
	for name, price := range map[string]*commonpb.Decimal{
		"zero":     dec(0, 0),
		"negative": dec(-15025, -2),
		"nil":      nil,
		"overflow": dec(9, 400), // 9e400 — +Inf once converted
	} {
		t.Run(name, func(t *testing.T) {
			var r recorder
			p := mustProvider(t, mark(price, time.Hour), r.observe())
			got, ok := p.Spot(context.Background(), "AAPL", t0)
			if ok || got != 0 {
				t.Fatalf("Spot = (%v, %v) for a %s price, want (0, false)", got, ok, name)
			}
			r.only(t, ReasonUnusablePrice)
		})
	}
}

func TestAZeroValuationTimeIsRefusedWithoutReadingTheStore(t *testing.T) {
	var r recorder
	f := mark(dec(100, 0), time.Hour)
	got, ok := mustProvider(t, f, r.observe()).Spot(context.Background(), "AAPL", time.Time{})
	if ok || got != 0 {
		t.Fatalf("Spot = (%v, %v) for a zero asOf, want (0, false) — a zero asOf means 'latest of "+
			"everything' to the store, which drops the knowledge horizon and leaks corrections "+
			"backward into the risk number", got, ok)
	}
	if f.calls != 0 {
		t.Errorf("the store was read %d times for a zero asOf; it must be refused before the read, "+
			"because the read itself is the unsafe act", f.calls)
	}
	r.only(t, ReasonNoAsOf)
}

// A MISCONFIGURATION MUST SURFACE AT STARTUP. Each refusal below would otherwise
// produce one uniform symptom at runtime — every option on the book skipped,
// forever — which is indistinguishable from a book holding no options.
func TestAProviderThatCouldNeverResolveAnythingIsRefusedAtConstruction(t *testing.T) {
	if _, err := FromStore(nil); !errors.Is(err, errNilStore) {
		t.Errorf("FromStore(nil) = %v, want errNilStore", err)
	}
	if _, err := FromStore(&fakeStore{}, WithPriceKind(store.PriceKindUnspecified)); !errors.Is(err, errUnknownKind) {
		t.Errorf("FromStore(kind=unspecified) = %v, want errUnknownKind — store.Observation.validate "+
			"refuses to persist an unspecified kind, so this provider would match zero rows for every "+
			"instrument that will ever exist", err)
	}
	if _, err := FromStore(&fakeStore{}, WithPriceKind(store.PriceKind(99))); !errors.Is(err, errUnknownKind) {
		t.Errorf("FromStore(kind=99) = %v, want errUnknownKind", err)
	}
	if _, err := FromStore(&fakeStore{}, WithMaxAge(0)); !errors.Is(err, errNonPositiveMaxAge) {
		t.Errorf("FromStore(maxAge=0) = %v, want errNonPositiveMaxAge — unbounded is a real decision "+
			"with a real cost and must be spelled, not arrived at by a zero-valued config field", err)
	}
	if _, err := FromStore(&fakeStore{}, WithMaxAge(-time.Hour)); !errors.Is(err, errNonPositiveMaxAge) {
		t.Errorf("FromStore(maxAge=-1h) = %v, want errNonPositiveMaxAge", err)
	}
	if _, err := FromStore(&fakeStore{}); err != nil {
		t.Errorf("FromStore with no options = %v, want a working provider on the defaults", err)
	}
}

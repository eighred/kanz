package vwap

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/marketdata/store"
)

// DID WE TRADE WORSE THAN THE MARKET DID (#436)?
//
// Shortfall — the OMS's measure — compares a fill to the DECISION-time mark. This
// compares it to what the average participant paid over the same window. The two
// disagree in the case that matters most, and TestSlippage_CatchesAnExecution...
// below is that case: an order that BEAT arrival while being worked worse than
// everyone else.

var (
	windowFrom = time.Date(2026, 8, 1, 11, 59, 0, 0, time.UTC)
	windowTo   = time.Date(2026, 8, 1, 12, 2, 0, 0, time.UTC)
	measuredAt = time.Date(2026, 8, 1, 12, 5, 0, 0, time.UTC)
)

func d(coef int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coef, Exponent: exp}
}

// fakeBars answers one window.
type fakeBars struct {
	bars []store.Bar
	err  error
	got  store.BarQuery
}

func (f *fakeBars) Bars(_ context.Context, q store.BarQuery) ([]store.Bar, error) {
	f.got = q
	return f.bars, f.err
}

// barAt is a 1-minute candle for the bucket `minute` minutes after windowFrom.
//
// THE BUCKET AND THE RESOLUTION ARE REAL, and that is not decoration since #591:
// coverage is read off them. A fixture leaving them zero describes a window whose
// every bucket is MISSING, so a test named for something else would quietly be a
// test of the gapped path.
func barAt(minute int, close, volume int64) store.Bar {
	return store.Bar{
		Resolution:  store.Resolution1m,
		BucketStart: windowFrom.Add(time.Duration(minute) * time.Minute),
		Close:       d(close, 0),
		Volume:      d(volume, 0),
	}
}

// wholeWindow is all three 1-minute buckets of [windowFrom, windowTo), each at
// close/volume — the fully covered case.
func wholeWindow(close, volume int64) []store.Bar {
	return []store.Bar{barAt(0, close, volume), barAt(1, close, volume), barAt(2, close, volume)}
}

func record(t *testing.T, fillPrice int64, withWindow bool) []byte {
	t.Helper()
	r := &orderpb.TransactionCostRecorded{
		OrderId: "o-1", FillId: "f-1", InstrumentId: "BTC-USD", Venue: "XBIN",
		Side: orderpb.Side_SIDE_BUY, FillPrice: d(fillPrice, 0),
		ArrivalPrice: d(100, 0), MeasuredAt: timestamppb.New(measuredAt),
	}
	if withWindow {
		r.ArrivalAt = timestamppb.New(windowFrom)
		r.ExecutedAt = timestamppb.New(windowTo)
	}
	b, err := proto.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func newWatch(t *testing.T, bars Bars) (*Watch, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	return New(reg, "__system__", bars, slog.New(slog.DiscardHandler),
		WithClock(func() time.Time { return measuredAt })), reg
}

func env() *envelopepb.Envelope {
	return &envelopepb.Envelope{EventId: "e-1", EventType: EventTypeCostRecorded, TenantId: "__system__"}
}

// THE CASE THAT JUSTIFIES THE WHOLE MEASURE.
//
// Arrival 100, bought at 95 — shortfall says −500 bps, a saving. But the market's
// own volume-weighted average over the window was 90, so this execution paid
// ABOVE what everyone else did. Shortfall alone calls this a win.
func TestSlippage_CatchesAnExecutionThatBeatArrivalButLostToTheMarket(t *testing.T) {
	// VWAP = (100·1 + 88·9)/10 = 892/10 = 89.2 ... use flat 90 for a clean number.
	bars := &fakeBars{bars: wholeWindow(90, 5)}
	w, reg := newWatch(t, bars)

	if err := w.Handle(context.Background(), env(), record(t, 95, true)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	n, sum := histogram(t, reg, "kanz_execution_vwap_slippage_bps", "XBIN")
	if n != 1 {
		t.Fatalf("observations = %d, want 1 — the join did not happen", n)
	}
	// (95 − 90)/90 · 10000 = 5000/9 ≈ 555.56 bps, POSITIVE: worse than the market.
	if sum < 555 || sum > 556 {
		t.Fatalf("slippage = %v bps, want ≈555.6 and POSITIVE — the fill paid above the "+
			"market's own average while beating arrival, which is precisely what shortfall "+
			"cannot see", sum)
	}
}

// THE WINDOW IS QUERIED, not invented. The bars must be the ones covering the
// order's working interval on ITS instrument and venue — a join against another
// venue's series would compare an execution to a market it never touched.
func TestSlippage_QueriesTheOrdersOwnWindow(t *testing.T) {
	bars := &fakeBars{bars: wholeWindow(90, 10)}
	w, _ := newWatch(t, bars)

	if err := w.Handle(context.Background(), env(), record(t, 95, true)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !bars.got.From.Equal(windowFrom) || !bars.got.To.Equal(windowTo) {
		t.Errorf("queried [%s, %s), want [%s, %s)", bars.got.From, bars.got.To, windowFrom, windowTo)
	}
	if bars.got.InstrumentID != "BTC-USD" || bars.got.Venue != "XBIN" {
		t.Errorf("queried %s@%s, want BTC-USD@XBIN", bars.got.InstrumentID, bars.got.Venue)
	}
	// THE KNOWLEDGE HORIZON IS SET, NOT LEFT ZERO. A zero AsOf means "now" by
	// default, and #428 records why relying on that default is dangerous.
	if bars.got.AsOf.IsZero() {
		t.Error("AsOf was left zero — the store's default is 'now', which is the unsafe default " +
			"#428 warns about; state the horizon rather than inheriting it")
	}
}

// A RECORD WITH NO WINDOW IS UNJOINABLE, NOT ZERO SLIPPAGE.
//
// An unset timestamp decodes as the EPOCH. Treating that as a window would select
// no bars — or worse, if any existed, compare an execution against the wrong
// century. And scoring it zero would report an unbenchmarked execution as one
// that matched the market exactly.
func TestSlippage_NoWindowIsCountedNotScoredZero(t *testing.T) {
	bars := &fakeBars{bars: wholeWindow(90, 10)}
	w, reg := newWatch(t, bars)

	if err := w.Handle(context.Background(), env(), record(t, 95, false)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if n, _ := histogramPresent(t, reg, "kanz_execution_vwap_slippage_bps", "XBIN"); n {
		t.Fatal("a record with no window was OBSERVED as slippage — an execution nobody could " +
			"benchmark must not read as one that matched the market exactly")
	}
	if got := counter(t, reg, "XBIN", "no_window"); got != 1 {
		t.Errorf("no_window count = %v, want 1", got)
	}
}

// A WINDOW WITH NO VOLUME HAS NO BENCHMARK. Nobody traded, so there is no average
// participant; inventing one would compare an execution to a market that was not
// there.
func TestSlippage_AZeroVolumeWindowIsUnjoinable(t *testing.T) {
	bars := &fakeBars{bars: wholeWindow(90, 0)}
	w, reg := newWatch(t, bars)

	if err := w.Handle(context.Background(), env(), record(t, 95, true)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := counter(t, reg, "XBIN", "no_volume"); got != 1 {
		t.Errorf("no_volume count = %v, want 1", got)
	}
}

// A BAR READ FAILURE IS ACKED AND COUNTED. Redelivering would recompute an
// analytic nothing acts on, and a bar series missing now will still be missing on
// the redelivery — bars are backfilled on their own schedule, not in response to
// this consumer.
func TestSlippage_ABarReadFailureIsAckedAndCounted(t *testing.T) {
	bars := &fakeBars{err: errors.New("db down")}
	w, reg := newWatch(t, bars)

	if err := w.Handle(context.Background(), env(), record(t, 95, true)); err != nil {
		t.Fatalf("Handle = %v, want nil (ack)", err)
	}
	if got := counter(t, reg, "XBIN", "bar_read_failed"); got != 1 {
		t.Errorf("bar_read_failed count = %v, want 1", got)
	}
}

// A RECORD FROM ANOTHER TENANT IS REFUSED.
func TestSlippage_CrossTenantIsRefused(t *testing.T) {
	w := New(prometheus.NewRegistry(), "acme", &fakeBars{}, slog.New(slog.DiscardHandler))
	e := env()
	e.TenantId = "someone-else"
	if err := w.Handle(context.Background(), e, record(t, 95, true)); err == nil {
		t.Fatal("a cost record from another tenant was joined into this tenant's series")
	}
}

// --- metric readers ---

func histogram(t *testing.T, reg *prometheus.Registry, name, venue string) (uint64, float64) {
	t.Helper()
	for _, f := range gather(t, reg) {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelled(m.GetLabel(), "venue", venue) && m.GetHistogram() != nil {
				return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
			}
		}
	}
	t.Fatalf("no %s series for venue %q", name, venue)
	return 0, 0
}

func histogramPresent(t *testing.T, reg *prometheus.Registry, name, venue string) (bool, bool) {
	t.Helper()
	for _, f := range gather(t, reg) {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelled(m.GetLabel(), "venue", venue) {
				return true, true
			}
		}
	}
	return false, false
}

func counter(t *testing.T, reg *prometheus.Registry, venue, reason string) float64 {
	t.Helper()
	for _, f := range gather(t, reg) {
		if f.GetName() != "kanz_execution_vwap_unjoinable_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelled(m.GetLabel(), "venue", venue) && labelled(m.GetLabel(), "reason", reason) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func gather(t *testing.T, reg *prometheus.Registry) []*dto.MetricFamily {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	return fams
}

func labelled(labels []*dto.LabelPair, name, value string) bool {
	for _, l := range labels {
		if l.GetName() == name && l.GetValue() == value {
			return true
		}
	}
	return false
}

// SLICED AND WHOLE EXECUTIONS ARE TOLD APART (#435, #483).
//
// This is what makes "does working an order on a schedule actually help?"
// answerable — the question #435 exists to let somebody settle. Without the
// label, every slice of a worked order lands in the same histogram as every
// order sent whole, and a venue's VWAP slippage blends two different execution
// strategies into one number that describes neither.
func TestSlippage_SlicedAndWholeExecutionsAreLabelledApart(t *testing.T) {
	bars := &fakeBars{bars: wholeWindow(90, 5)}
	w, reg := newWatch(t, bars)

	// An ordinary order, sent whole.
	if err := w.Handle(context.Background(), env(), record(t, 95, true)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// A slice of a worked parent.
	if err := w.Handle(context.Background(), env(), childRecord(t, 95)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	whole := labelledHistogram(t, reg, "XBIN", "whole")
	sliced := labelledHistogram(t, reg, "XBIN", "sliced")
	if whole != 1 {
		t.Errorf("worked=whole observations = %d, want 1", whole)
	}
	if sliced != 1 {
		t.Errorf("worked=sliced observations = %d, want 1 — a slice of a parent order is "+
			"indistinguishable from an order sent whole, so whether slicing helps cannot be "+
			"answered from this data", sliced)
	}
}

// childRecord is a cost record for a SLICE of a worked parent order.
func childRecord(t *testing.T, fillPrice int64) []byte {
	t.Helper()
	r := &orderpb.TransactionCostRecorded{
		OrderId: "p1:0", ParentOrderId: "p1", FillId: "f-2", InstrumentId: "BTC-USD",
		Venue: "XBIN", Side: orderpb.Side_SIDE_BUY, FillPrice: d(fillPrice, 0),
		ArrivalPrice: d(100, 0), MeasuredAt: timestamppb.New(measuredAt),
		ArrivalAt: timestamppb.New(windowFrom), ExecutedAt: timestamppb.New(windowTo),
	}
	b, err := proto.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func labelledHistogram(t *testing.T, reg *prometheus.Registry, venue, worked string) uint64 {
	t.Helper()
	for _, f := range gather(t, reg) {
		if f.GetName() != "kanz_execution_vwap_slippage_bps" {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelled(m.GetLabel(), "venue", venue) && labelled(m.GetLabel(), "worked", worked) &&
				m.GetHistogram() != nil {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

// ===== THE BENCHMARK SAYS HOW MUCH OF ITS WINDOW IT SAW (#591) =====

// THE COVERED CASE IS LABELLED COVERED, which is what makes `partial` worth
// reading. Three minutes of window, three bars, nothing missing.
func TestSlippage_AWholeWindowIsLabelledWhole(t *testing.T) {
	bars := &fakeBars{bars: wholeWindow(90, 5)}
	w, reg := newWatch(t, bars)

	if err := w.Handle(context.Background(), env(), record(t, 95, true)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := coverageCount(t, reg, "XBIN", "whole"); got != 1 {
		t.Errorf("coverage=whole observations = %d, want 1", got)
	}
	if got := coverageCount(t, reg, "XBIN", "partial"); got != 0 {
		t.Errorf("coverage=partial observations = %d, want 0", got)
	}
}

// THE DEFECT THIS CLOSES. bars/fold.go emits NO BAR for a minute in which nothing
// traded, so two thirds of this window are absent — and the measure still
// produces a number, because the benchmark is the VWAP of the third that
// survived. unjoinable{reason="no_volume"} never saw it: that only ever fired on
// a window that was ENTIRELY empty.
//
// The figure is not suppressed — a post-trade measurement blanked on every quiet
// instrument is worth nothing, and on the 1m series quiet is normal. It is
// LABELLED, so a query cannot reach the slippage without reaching what it was
// computed over.
func TestSlippage_APartialWindowIsLabelledRatherThanReadAsWhole(t *testing.T) {
	// Only the 11:59 bucket has a bar. 12:00 and 12:01 are gone.
	bars := &fakeBars{bars: []store.Bar{barAt(0, 90, 10)}}
	w, reg := newWatch(t, bars)

	if err := w.Handle(context.Background(), env(), record(t, 95, true)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := coverageCount(t, reg, "XBIN", "partial"); got != 1 {
		t.Fatalf("coverage=partial observations = %d, want 1 — a benchmark over one third of "+
			"its window was published as if the window were whole", got)
	}
	if got := coverageCount(t, reg, "XBIN", "whole"); got != 0 {
		t.Errorf("coverage=whole observations = %d, want 0", got)
	}
	// AND THE NUMBER IS STILL THERE. Refusing it would blank TCA on every quiet
	// instrument, which is the repair that costs more than the defect.
	if _, sum := histogram(t, reg, "kanz_execution_vwap_slippage_bps", "XBIN"); sum < 555 || sum > 556 {
		t.Errorf("slippage = %v bps, want the ≈555.6 measured against the surviving minute — "+
			"the label qualifies the figure, it does not replace it", sum)
	}
}

// A WINDOW SHORTER THAN ONE BAR IS NOT A COVERED WINDOW. store.Window.Whole() is
// VACUOUSLY true over zero buckets and says so itself, so inheriting that answer
// would publish the least-covered case as the best-covered one.
//
// IT IS REACHABLE, not defensive: BarQuery bounds From/To on BucketStart —
// inclusive/exclusive — so a fill worked from 12:00:00 to 12:00:30 selects the
// 12:00 bar while containing no whole bucket, and the benchmark comes from a
// minute that outlasts the window it is meant to describe.
func TestSlippage_AWindowShorterThanABarIsNotLabelledWhole(t *testing.T) {
	minute := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	bars := &fakeBars{bars: []store.Bar{{
		Resolution: store.Resolution1m, BucketStart: minute,
		Close: d(90, 0), Volume: d(10, 0),
	}}}
	w, reg := newWatch(t, bars)

	rec := recordOver(t, 95, minute, minute.Add(30*time.Second))
	if err := w.Handle(context.Background(), env(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := coverageCount(t, reg, "XBIN", "whole"); got != 0 {
		t.Fatalf("coverage=whole observations = %d, want 0 — a window holding no complete "+
			"bucket was published as fully covered", got)
	}
	if got := coverageCount(t, reg, "XBIN", "shorter_than_a_bar"); got != 1 {
		t.Errorf("coverage=shorter_than_a_bar observations = %d, want 1", got)
	}
}

// recordOver is a cost record worked over an arbitrary window.
func recordOver(t *testing.T, fillPrice int64, from, to time.Time) []byte {
	t.Helper()
	r := &orderpb.TransactionCostRecorded{
		OrderId: "o-2", FillId: "f-3", InstrumentId: "BTC-USD", Venue: "XBIN",
		Side: orderpb.Side_SIDE_BUY, FillPrice: d(fillPrice, 0),
		ArrivalPrice: d(100, 0), MeasuredAt: timestamppb.New(measuredAt),
		ArrivalAt: timestamppb.New(from), ExecutedAt: timestamppb.New(to),
	}
	b, err := proto.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// coverageCount reads the slippage observations carrying one coverage label.
func coverageCount(t *testing.T, reg *prometheus.Registry, venue, coverage string) uint64 {
	t.Helper()
	for _, f := range gather(t, reg) {
		if f.GetName() != "kanz_execution_vwap_slippage_bps" {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelled(m.GetLabel(), "venue", venue) &&
				labelled(m.GetLabel(), "coverage", coverage) && m.GetHistogram() != nil {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

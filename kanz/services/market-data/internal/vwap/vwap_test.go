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

func bar(close, volume int64) store.Bar {
	return store.Bar{Close: d(close, 0), Volume: d(volume, 0)}
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
	bars := &fakeBars{bars: []store.Bar{bar(90, 5), bar(90, 5)}}
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
	bars := &fakeBars{bars: []store.Bar{bar(90, 10)}}
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
	bars := &fakeBars{bars: []store.Bar{bar(90, 10)}}
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
	bars := &fakeBars{bars: []store.Bar{bar(90, 0)}}
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

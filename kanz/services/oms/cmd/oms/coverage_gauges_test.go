package main

// THE EXECUTION-QUALITY COVERAGE SIGNALS, EXERCISED AT THE COMPOSITION ROOT.
//
// Two alerts in infra/observability/alerts/operational.rules.yaml are written
// directly over the four gauges and one counter built here, and an alert is only
// as good as the series under it. The rules are proven to fire by promtool
// against SYNTHETIC series — which proves the expression, and proves nothing
// about whether this process ever produces series of that shape. That gap is
// exactly how #62's ten data-quality rules came to parse, deploy and fire never.
//
// So these tests close the other half: they fold real market events through the
// real mark source, gather the real registry, and read the numbers an alert
// would evaluate. Between the two, an alert over these metrics is proven end to
// end without a cluster.
//
// They also exist because this file's subjects live in a composition root. The
// four gauges were inline in runConsumers until #875 — a function that takes a
// bus connection and a database, and is therefore unreachable from any test —
// which is where this platform has twice shipped a startup crash under a fully
// green suite.

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/marketdata/mark"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/oms/internal/order"
)

var coverageBase = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

// testProvider is a Provider carrying nothing but a fresh registry — the only
// field either builder under test touches. observability.New would install a
// global OTel TracerProvider as a side effect, which is not something a metric
// test should do to the rest of the package's tests.
func testProvider() *observability.Provider {
	return &observability.Provider{Registry: prometheus.NewRegistry()}
}

// gaugeValue lives in outbox_test.go — one reader for one concept. Its absence
// arm is what makes every "want 0" assertion below meaningful: an unregistered
// metric must fail, not read as zero.

func marketEnvelope(eventType string) *envelopepb.Envelope {
	return &envelopepb.Envelope{EventTime: timestamppb.New(coverageBase), EventType: eventType}
}

func foldTradeEvent(t *testing.T, s *mark.Source, instrument string, coeff int64, at time.Time) {
	t.Helper()
	b, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: instrument,
		EventTime:    timestamppb.New(at),
		Data: &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{
			Price: &commonpb.Decimal{Coefficient: coeff, Exponent: 0},
		}},
	})
	if err != nil {
		t.Fatalf("marshal trade: %v", err)
	}
	if err := s.Handle(context.Background(), marketEnvelope("market.crypto.trade"), b); err != nil {
		t.Fatalf("Handle trade: %v", err)
	}
}

func foldQuoteEvent(t *testing.T, s *mark.Source, instrument string, bid, ask int64, at time.Time) {
	t.Helper()
	b, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: instrument,
		EventTime:    timestamppb.New(at),
		Data: &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{
			BidPrice: &commonpb.Decimal{Coefficient: bid, Exponent: 0},
			AskPrice: &commonpb.Decimal{Coefficient: ask, Exponent: 0},
		}},
	})
	if err != nil {
		t.Fatalf("marshal quote: %v", err)
	}
	if err := s.Handle(context.Background(), marketEnvelope("market.crypto.quote"), b); err != nil {
		t.Fatalf("Handle quote: %v", err)
	}
}

// THE CONDITION OMSQuoteCoverageAbsent EXISTS FOR, PRODUCED BY THE REAL FOLD.
//
// A trade-only spine — venue-binance and venue-okx both poll a last-price
// ticker and publish market.crypto.trade — gives the OMS live marks for every
// instrument it trades and a quoted width for none of them. That is a fully
// healthy price feed on which every execution attribution is TOTAL_ONLY, and
// before these gauges it was indistinguishable from a spine that quoted
// everything.
func TestATradeOnlySpineReportsMarkCoverageAndNoQuoteCoverage(t *testing.T) {
	obs := testProvider()
	marks := mark.New(func() time.Time { return coverageBase }, 30*time.Second)
	registerMarkCoverageGauges(obs, marks)

	foldTradeEvent(t, marks, "BTC-USD", 60000, coverageBase)
	foldTradeEvent(t, marks, "ETH-USD", 3000, coverageBase)

	if got := gaugeValue(t, obs.Registry, "kanz_oms_mark_instruments_live"); got != 2 {
		t.Fatalf("kanz_oms_mark_instruments_live = %v, want 2 — the price feed is healthy", got)
	}
	if got := gaugeValue(t, obs.Registry, "kanz_oms_quote_instruments_live"); got != 0 {
		t.Fatalf("kanz_oms_quote_instruments_live = %v, want 0 — a trade print carries no width, so "+
			"a trade-only spine must report zero quote coverage. Anything else means the alert "+
			"OMSQuoteCoverageAbsent can never fire on the estate it was written for", got)
	}
	if got := gaugeValue(t, obs.Registry, "kanz_oms_quote_instruments_held"); got != 0 {
		t.Fatalf("kanz_oms_quote_instruments_held = %v, want 0", got)
	}
}

// AND THE OTHER DIRECTION: a quoted spine must clear the alert. Stated because
// a gauge hard-wired to zero would pass the case above perfectly, and the alert
// built on it would then fire forever on a healthy estate — which is how an
// alerting layer trains its readers to ignore it (alerts/README.md).
func TestAQuotedSpineReportsQuoteCoverage(t *testing.T) {
	obs := testProvider()
	marks := mark.New(func() time.Time { return coverageBase }, 30*time.Second)
	registerMarkCoverageGauges(obs, marks)

	foldQuoteEvent(t, marks, "BTC-USD", 59990, 60010, coverageBase)
	foldTradeEvent(t, marks, "ETH-USD", 3000, coverageBase)

	if got := gaugeValue(t, obs.Registry, "kanz_oms_mark_instruments_live"); got != 2 {
		t.Fatalf("kanz_oms_mark_instruments_live = %v, want 2 — a quote sets a mark from its mid", got)
	}
	if got := gaugeValue(t, obs.Registry, "kanz_oms_quote_instruments_live"); got != 1 {
		t.Fatalf("kanz_oms_quote_instruments_live = %v, want 1 — only BTC-USD was quoted, and "+
			"partial coverage must read as partial", got)
	}
}

// A STOPPED QUOTE FEED, WHICH IS THE OTHER HALF OF held − live.
//
// The width ages out with no fold to sweep it, so held stays at its high-water
// mark while live falls to zero. An operator reading held == live == 0 is
// looking at a spine that never quoted these instruments; held > 0 with live == 0
// is a feed that ran and stopped. The gauges must be able to say which.
func TestAStoppedQuoteFeedShowsHeldWithoutLive(t *testing.T) {
	obs := testProvider()
	now := coverageBase
	marks := mark.New(func() time.Time { return now }, 30*time.Second)
	registerMarkCoverageGauges(obs, marks)

	foldQuoteEvent(t, marks, "BTC-USD", 59990, 60010, coverageBase)
	if got := gaugeValue(t, obs.Registry, "kanz_oms_quote_instruments_live"); got != 1 {
		t.Fatalf("kanz_oms_quote_instruments_live = %v, want 1 before the feed stops", got)
	}

	now = coverageBase.Add(time.Hour) // the feed stops: nothing folds, nothing sweeps

	if got := gaugeValue(t, obs.Registry, "kanz_oms_quote_instruments_held"); got != 1 {
		t.Fatalf("kanz_oms_quote_instruments_held = %v, want 1 — the unswept width is still held, "+
			"and that is what distinguishes a stopped feed from a cold one", got)
	}
	if got := gaugeValue(t, obs.Registry, "kanz_oms_quote_instruments_live"); got != 0 {
		t.Fatalf("kanz_oms_quote_instruments_live = %v, want 0 — the width is an hour past "+
			"OMS_PRICE_MAX_AGE and Touch refuses it, so reporting it as coverage would keep "+
			"OMSQuoteCoverageAbsent silent through the entire outage", got)
	}
}

// EVERY OUTCOME SERIES EXISTS AT ZERO BEFORE THE FIRST ORDER (#875).
//
// This is the property the alert OMSExecutionAttributionUndecomposable depends
// on and cannot assert for itself. Its expression is "total_only is rising AND
// decomposed is not", and the second half compares against a series that, on an
// unseeded CounterVec, does not exist until something increments it. An empty
// vector makes the `and` unmatched and the rule silent — in precisely the state
// it was written to detect.
func TestEveryAttributionOutcomeIsSeededAtZero(t *testing.T) {
	obs := testProvider()
	executionAttributionCounter(obs)

	families, err := obs.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	seen := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "kanz_oms_execution_attributions_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			seen[labelValue(m, "outcome")] = m.GetCounter().GetValue()
		}
	}

	// NON-VACUITY: the counter must be registered at all. Without this arm an
	// unregistered counter passes every check below, because `seen` would be
	// empty and so would the set of outcomes it fails to contain.
	if len(seen) == 0 {
		t.Fatal("kanz_oms_execution_attributions_total exports no series at all — the counter is " +
			"not registered, or the seeding loop was removed. Two alert rules read this metric")
	}
	if len(order.AttributionOutcomes) == 0 {
		t.Fatal("order.AttributionOutcomes is empty — the seed list is the truth set for this test, " +
			"and an empty one asserts nothing")
	}

	for _, outcome := range order.AttributionOutcomes {
		v, ok := seen[outcome]
		if !ok {
			t.Errorf("outcome %q exports NO series before its first increment.\n\n"+
				"A rule comparing against it gets an empty vector and cannot fire. This is the "+
				"whole reason the counter is seeded — see executionAttributionCounter.", outcome)
			continue
		}
		if v != 0 {
			t.Errorf("outcome %q is seeded at %v, want 0 — seeding must create the series without "+
				"claiming the outcome occurred", outcome, v)
		}
	}
	if len(seen) != len(order.AttributionOutcomes) {
		t.Errorf("counter exports %d series but AttributionOutcomes lists %d — a series nothing can "+
			"increment sits at zero forever and reads as \"measured, never happened\"",
			len(seen), len(order.AttributionOutcomes))
	}
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

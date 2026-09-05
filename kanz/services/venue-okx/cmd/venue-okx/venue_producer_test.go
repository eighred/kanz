package main

// THE COMPOSITION ROOT IS WHERE THIS DEFECT LIVED (#245).
//
// okx_connector_test.go proves the ticker feed publishes a MarketDataEvent —
// through okxCapture, an EVENT-level double that accepts any bus.Event and
// therefore never runs bus.Validate. serve() built the real producer with
// {Source, ProducerVersion, Metrics} and no Tenant, so the wired ticker was
// refused "tenant_id required" on every poll while the package test stayed
// green. That is exactly the split publisher_validation_test.go describes, and
// services/venue-okx/internal/okx is named on its exemption list.
//
// These tests sit at the composition root because that is the layer the defect
// was at: nothing inside internal/okx could see it, and serve() dials
// OKX and a broker before it constructs anything.

import (
	"context"
	"errors"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/venue-okx/internal/config"
)

// recordingClient is a bus.Client — the MESSAGE-level double (#245 Tier-B). The
// real bus.Producer sits above it, so bus.Validate runs on every envelope.
type recordingClient struct{ sent []bus.Message }

func (c *recordingClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *recordingClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *recordingClient) Close() error { return nil }

// tickerEvent is the ticker feed's envelope, as okx_connector.go builds it:
// no Event.TenantID, because the feed relies on the producer's fallback.
func tickerEvent() bus.Event {
	return bus.Event{
		Subject: "market.crypto.trade", EventType: "market.crypto.trade",
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "market",
		EventTime: time.Now().UTC(), PartitionKey: "BTC-USD",
		Payload: &marketpb.MarketDataEvent{
			InstrumentId: "BTC-USD", Symbol: "BTC-USDT", Mic: "OKX",
			EventTime: timestamppb.Now(),
			Data: &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{
				Price: &commonpb.Decimal{Coefficient: 501235, Exponent: -1},
			}},
		},
	}
}

// A MARK PUBLISHED THROUGH THE PRODUCER WIRED THE WAY serve() WIRES IT MUST
// REACH THE TRANSPORT.
//
// The ticker feed stamps no per-event tenant and runs off a time.Ticker, so
// ProducerConfig.Tenant is its only source. Before it was set this failed with
// "envelope validation: tenant_id required".
func TestVenueProducerWiredFromConfigEmitsAValidatedMark(t *testing.T) {
	cfg := config.Config{Source: "venue-okx", Tenant: "acme-fund"}

	rc := &recordingClient{}
	producer, err := bus.NewProducer(rc, venueProducerConfig(cfg, nil))
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}

	if err := producer.Publish(context.Background(), tickerEvent()); err != nil {
		t.Fatalf("a ticker mark published through the wired producer was refused: %v\n"+
			"This is the composition root, not the connector: tv-sync's MarkSource gets nothing "+
			"and unrealized P&L stops moving.", err)
	}
	if len(rc.sent) != 1 {
		t.Fatalf("transport received %d messages, want 1", len(rc.sent))
	}
	env, _, err := bus.Unframe(rc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Errorf("wired envelope fails Validate: %v", err)
	}
	if env.GetTenantId() != "acme-fund" {
		t.Errorf("TenantId = %q, want the configured VENUE_OKX_TENANT (acme-fund)", env.GetTenantId())
	}
}

// THE SILENT FAILURE HAD A LOUD CONSEQUENCE, and this is it.
//
// serve() wraps this producer in a HealthPublisher and hands the SAME instance
// to readiness.TrackPublisher and to WorkerDeps.Publisher. The ticker discards
// its publish error (`_ = f.pub.Publish(...)`), but HealthPublisher already
// counted it — and DefaultPublishFailureThreshold is 3, so one poll over three
// instruments was enough. The adapter reported NOT READY, dropped out of its
// Service, and the OMS router hard-errored on this MIC while order execution
// itself was working perfectly.
func TestVenueProducerWiredFromConfigKeepsReadinessHealthy(t *testing.T) {
	cfg := config.Config{Source: "venue-okx", Tenant: "acme-fund"}
	producer, err := bus.NewProducer(&recordingClient{}, venueProducerConfig(cfg, nil))
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	health := bus.NewHealthPublisher(producer, bus.DefaultPublishFailureThreshold)

	// One ticker poll over three instruments, the way runTicker drives it.
	for range 3 {
		_ = health.Publish(context.Background(), tickerEvent())
	}
	ok, consecutive, lastErr := health.Status()
	if !ok {
		t.Fatalf("one ticker poll marked the adapter NOT READY: %d consecutive publish failures, last: %v\n"+
			"readiness.TrackPublisher watches this publisher, so the pod leaves its Service and the OMS "+
			"router hard-errors on this MIC — a total execution outage caused by the mark feed.",
			consecutive, lastErr)
	}
}

// The mark producer's tenant must be the SERVICE's tenant — the same one the
// fill and StateHealed emitters stamp explicitly (deps.Tenant, also cfg.Tenant)
// and the same one the RLS pool is pinned to. A mark under a different tenant is
// refused by a tenant-dedicated consumer's bus.RequireTenantScope (#223).
func TestVenueProducerTenantIsTheServiceTenant(t *testing.T) {
	cfg := config.Config{Source: "venue-okx", Tenant: "beta-fund"}
	if got := venueProducerConfig(cfg, nil).Tenant; got != cfg.Tenant {
		t.Fatalf("venue producer tenant = %q, want the service tenant %q — the ticker feed has no "+
			"other source of one", got, cfg.Tenant)
	}
}

// THE COUNTER MUST SURVIVE REGISTRATION, BECAUSE THE ALTERNATIVE IS A CRASH LOOP
// (#673). obs.Registry.MustRegister runs inside serve(), after config load and
// before anything serves traffic — a bad metric name or a label that collides
// with a const label panics there, and the pod restarts forever with a config
// nobody changed. Nothing else in this package reaches that line: serve() dials
// the exchange and a broker first. This asserts the collector on a throwaway
// registry, which is the same validation MustRegister performs.
func TestMarkTickDroppedCounterRegistersAndCounts(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(markTickDropped)

	markTickDropped.WithLabelValues("OKX", "BTC-USD").Inc()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var got *dto.MetricFamily
	for _, f := range families {
		if f.GetName() == "kanz_venue_mark_tick_dropped_total" {
			got = f
		}
	}
	if got == nil {
		t.Fatal("kanz_venue_mark_tick_dropped_total exports no series after an increment — a registered " +
			"collector nobody writes reads as ZERO on every dashboard and can never fire an alert")
	}
	labels := map[string]string{}
	for _, l := range got.GetMetric()[0].GetLabel() {
		labels[l.GetName()] = l.GetValue()
	}
	for name, want := range map[string]string{"venue": "okx", "mic": "OKX", "instrument": "BTC-USD"} {
		if labels[name] != want {
			t.Errorf("label %q = %q, want %q. These three labels are what this counter has that "+
				"kanz_bus_publish_total{subject,result} does not: WHICH venue, WHICH exchange, and WHICH "+
				"instrument went dark while the others kept publishing", name, labels[name], want)
		}
	}
	if v := got.GetMetric()[0].GetCounter().GetValue(); v != 1 {
		t.Errorf("counter = %v after one drop, want 1", v)
	}
}

// THE REFUSAL COUNTER MUST SURVIVE REGISTRATION AND EXPORT A SERIES (#1045).
//
// Two failures in one, and this estate has shipped the second twice. A bad
// metric name or a label colliding with a const label panics inside
// obs.Registry.MustRegister and crash-loops the pod on a config nobody changed;
// and a CounterVec whose labels are never written exports NO SERIES AT ALL, so
// an alert over it compares a threshold against an empty vector and can never
// fire — silent in exactly the state it was written for.
//
// The collector is registered in serve() beside the other package-level ones,
// BEFORE the exchange and the broker are dialled and not inside any branch that
// depends on either. That placement is what this pairs with: a collector
// registered on one arm of an `if` is the shape that goes silent.
func TestFillRefusedCounterRegistersAndCounts(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(fillRefused)

	fillRefused.WithLabelValues("overfill").Inc()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var got *dto.MetricFamily
	for _, f := range families {
		if f.GetName() == "kanz_venue_fill_refused_total" {
			got = f
		}
	}
	if got == nil {
		t.Fatal("kanz_venue_fill_refused_total exports no series after an increment — an over-fill " +
			"the adapter refused would then be indistinguishable from a venue that never over-filled")
	}
	labels := map[string]string{}
	for _, l := range got.GetMetric()[0].GetLabel() {
		labels[l.GetName()] = l.GetValue()
	}
	for name, want := range map[string]string{"venue": "okx", "reason": "overfill"} {
		if labels[name] != want {
			t.Errorf("label %q = %q, want %q — the venue says WHICH exchange disagreed with the "+
				"platform, and the reason separates an over-fill from a number that would not convert",
				name, labels[name], want)
		}
	}
}

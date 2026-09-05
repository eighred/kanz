package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/pkg/bus"
)

// THE COMPOSITION ROOT'S OWN WIRING, ASSERTED (#1037).
//
// app.MeasureMethodPosture can be unit-tested and publish.Publisher can be
// unit-tested, and BOTH stay green while this file hands the publisher no
// observer at all — the gauge then reads method="unobserved" forever on a pod
// that is announcing a placeholder VaR on every recompute. That mutation survived
// the whole suite once; it is why newMeasurePublisher is a named builder that
// RETURNS what it built instead of six lines inside runEngine.

// discardClient accepts every publish and keeps nothing. The assertion here is
// about the gauge, not the bytes — internal/risk/publish/provenance_test.go owns
// the wire shape.
type discardClient struct{ published int }

func (d *discardClient) Publish(context.Context, bus.Message) error { d.published++; return nil }
func (d *discardClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (d *discardClient) Close() error { return nil }

func TestNewMeasurePublisher_TheAnnouncedModelReachesTheGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	producer, err := bus.NewProducer(&discardClient{}, bus.ProducerConfig{
		Source: "risk-engine/test", ProducerVersion: "0.0.0", Tenant: "acme",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}

	publisher, methods, err := newMeasurePublisher(producer, reg)
	if err != nil {
		t.Fatalf("newMeasurePublisher: %v", err)
	}
	// The registry an engine with no RISK_ENGINE_MARKETDATA_DATABASE_URL serves,
	// which is every manifest in infra/.
	registry := compute.DefaultRegistry()
	methods.Seed(registry)

	asOf := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	p := domain.NewPortfolio("PF-1", "USD")
	p.SetAggregate(domain.AggregateUpdate{AsOf: asOf, BaseCurrency: "USD"})
	p.SetPosition(domain.Position{
		InstrumentID: "AAPL",
		MarketValue:  &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 1000}, CurrencyCode: "USD"},
		AsOf:         asOf,
	})
	if err := publisher.EmitMeasures(context.Background(), compute.ComputeMeasures(p, registry, nil), nil); err != nil {
		t.Fatalf("EmitMeasures: %v", err)
	}

	got := gaugeMethods(t, reg)
	for measure, method := range map[string]string{
		"VaR99": "placeholder_1pct_gross",
		"Delta": "net_exposure_placeholder",
	} {
		if got[measure+"|"+method] != 1 {
			t.Errorf("kanz_risk_measure_method{measure=%q,method=%q} = %v, want 1\n\n"+
				"The engine announced this model on risk.portfolio.measures_computed and the "+
				"gauge did not move, so this pod is serving an illustrative constant as its "+
				"headline risk number with no series saying so.",
				measure, method, got[measure+"|"+method])
		}
		if got[measure+"|unobserved"] != 0 {
			t.Errorf("kanz_risk_measure_method{measure=%q,method=\"unobserved\"} = %v, want 0 — "+
				"the seeded series must be superseded, not left reading true beside the real one",
				measure, got[measure+"|unobserved"])
		}
	}
}

// gaugeMethods reads kanz_risk_measure_method as measure|method -> value.
func gaugeMethods(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "kanz_risk_measure_method" {
			continue
		}
		for _, m := range f.GetMetric() {
			var measure, method string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "measure":
					measure = l.GetValue()
				case "method":
					method = l.GetValue()
				}
			}
			out[measure+"|"+method] = m.GetGauge().GetValue()
		}
	}
	if len(out) == 0 {
		t.Fatal("kanz_risk_measure_method exported NO series at all — the collector was never " +
			"registered, and an alert over it evaluates to no-data rather than to false")
	}
	return out
}

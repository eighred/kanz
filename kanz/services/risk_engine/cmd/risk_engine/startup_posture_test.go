package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/risk_engine/internal/app"
)

func TestStartupPosturesShareTheFinalRegistryAndObserver(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfigured", true: "configured"}[configured], func(t *testing.T) {
			reg := prometheus.NewRegistry()
			postures := newStartupPostures(reg, "risk-engine/test", configured)
			before := gaugeMethods(t, reg)
			if len(before) != len(compute.Catalogue()) {
				t.Fatalf("startup method inventory = %d", len(before))
			}
			for _, measure := range compute.Catalogue() {
				if before[string(measure.Name)+"|"+app.MethodUnconfigured] != 1 {
					t.Fatalf("missing startup %s", measure.Name)
				}
			}
			if !configured {
				return
			}
			registry := compute.DefaultRegistry()
			stateMeasurePostures(slog.New(slog.NewTextHandler(io.Discard, nil)), registry, postures, app.NewGreekModelPosture(reg))
			postures.methods.Observe("VaR99", "test_announced_method")
			after := gaugeMethods(t, reg)
			if after["VaR99|test_announced_method"] != 1 || after["VaR99|"+app.MethodUnobserved] != 0 || after["VaR99|"+app.MethodUnconfigured] != 0 {
				t.Fatalf("observer did not update original startup registry: %v", after)
			}
			if testutil.CollectAndCount(reg, "kanz_risk_measure_registry_ready") != 1 {
				t.Fatal("missing registry readiness")
			}
		})
	}
}

func TestMeasurePublisherRefusesMissingStartupDependencies(t *testing.T) {
	if _, err := newMeasurePublisher(nil, app.NewMeasureMethodPosture(prometheus.NewRegistry())); err == nil {
		t.Fatal("nil producer accepted")
	}
	producer, err := bus.NewProducer(&discardClient{}, bus.ProducerConfig{Source: "risk-engine/test", ProducerVersion: "test", Tenant: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newMeasurePublisher(producer, nil); err == nil {
		t.Fatal("missing startup observer accepted")
	}
}

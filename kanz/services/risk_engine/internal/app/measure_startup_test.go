package app

import (
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
)

func TestMeasureStartupIsExplicitBeforeAnyRegistryExists(t *testing.T) {
	reg := prometheus.NewRegistry()
	live := NewMeasureRegistryPosture(reg)
	methods := NewMeasureMethodPosture(reg)
	methods.Unconfigured()
	if got := testutil.ToFloat64(live.ready); got != 0 {
		t.Fatalf("unwired registry ready = %v", got)
	}
	series := methodSeries(t, reg)
	if len(series) != len(compute.Catalogue()) {
		t.Fatalf("unconfigured method count = %d", len(series))
	}
	for _, measure := range compute.Catalogue() {
		value, exists := seriesFor(t, reg, string(measure.Name))
		if !exists || value != 0 {
			t.Fatalf("unwired %s live = %v, present %v", measure.Name, value, exists)
		}
		if series[string(measure.Name)+"|"+MethodUnconfigured] != 1 {
			t.Fatalf("missing unconfigured method for %s", measure.Name)
		}
	}
	// Missing input cannot promote an unknown registry into an evaluated one.
	live.State(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	methods.Seed(nil)
	if testutil.ToFloat64(live.ready) != 0 {
		t.Fatal("nil registry became ready")
	}
}

func TestMeasureStartupStatesTheSuppliedRegistryWithoutExecutingMeasures(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty_registry", true: "default_registry"}[populated], func(t *testing.T) {
			reg := prometheus.NewRegistry()
			live := NewMeasureRegistryPosture(reg)
			methods := NewMeasureMethodPosture(reg)
			methods.Unconfigured()
			registry := compute.NewRegistry()
			if populated {
				registry = compute.DefaultRegistry()
				registry.Register("GrossExposure", func(*domain.Portfolio) v1.Measure {
					t.Fatal("posture executed a risk measure")
					return v1.Measure{}
				})
			}
			live.State(slog.New(slog.NewTextHandler(io.Discard, nil)), registry)
			methods.Seed(registry)
			if testutil.ToFloat64(live.ready) != 1 {
				t.Fatal("supplied registry not stated")
			}
			served := map[string]bool{}
			for _, name := range registry.Names() {
				served[string(name)] = true
			}
			series := methodSeries(t, reg)
			for _, measure := range compute.Catalogue() {
				name := string(measure.Name)
				value, exists := seriesFor(t, reg, name)
				want := 0.0
				if served[name] {
					want = 1
				}
				if !exists || value != want {
					t.Fatalf("%s live = %v, present %v, want %v", name, value, exists, want)
				}
				if series[name+"|"+MethodUnconfigured] != 0 {
					t.Fatalf("%s remained unconfigured", name)
				}
				if series[name+"|"+MethodUnobserved] != want {
					t.Fatalf("%s did not reflect supplied registry", name)
				}
			}
			for key, value := range series {
				if value != 0 && value != 1 {
					t.Fatalf("invalid posture value for %s", key)
				}
			}
		})
	}
}

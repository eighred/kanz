package bus_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/kanz-eng/kanz/pkg/bus"
)

// installTracing sets a real (in-memory) tracer + the W3C propagator for the
// test, returning the recorded spans. Mirrors what observability.New installs.
//
// It RESTORES the previous global provider and propagator afterwards. otel's
// setters are process-global and sticky — Shutdown()ing the provider does not
// uninstall it — so a test that installs one and walks away leaves every LATER
// test in the process running against a live tracer.
//
// That is not hypothetical: it silently broke the two lineage tests, which assert
// the LEGACY-STRING branch of the producer's documented trace precedence (explicit
// field > active OTel span > legacy string ctx). With a provider still installed,
// the producer opens a real span, the OTel branch wins, and the legacy string is
// never reached. They passed on the first pass and failed on the second — the
// classic shape of a global leaked between tests, and the reason `-count=2` was red.
func installTracing(t *testing.T) *sdktrace.TracerProvider {
	t.Helper()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	t.Cleanup(func() {
		// UNINSTALL, explicitly — and with FRESH no-op values, never with whatever
		// otel.Get*() returned beforehand. Both getters hand back a process-global
		// DELEGATING wrapper whose delegate is set exactly once (a sync.Once), so
		// "restoring" them re-installs a wrapper that still delegates to the SDK. The
		// only way to turn tracing back off is to install something that does nothing.
		//
		// The propagator is the one that actually matters here: the producer's trace
		// stamp comes from observability.TraceparentFromContext, which INJECTS through
		// the global propagator — with an empty composite it writes nothing, the
		// traceparent is empty, and the producer falls through to the legacy-string
		// branch the lineage tests assert. (The tracer provider cannot be truly
		// uninstalled at all: pkg/bus caches its Tracer in a package var at init, and
		// that handle's delegate is bound for the life of the process. Which is exactly
		// why this cleanup must exist — no later test can undo it.)
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
		_ = tp.Shutdown(context.Background())
	})
	return tp
}

// TestPublishStampsActiveSpanTrace: with tracing installed, Publish opens a
// producer span and stamps its W3C traceparent into the envelope — the wire
// half of OBS-01d.
func TestPublishStampsActiveSpanTrace(t *testing.T) {
	installTracing(t)
	p, cc := newTestProducer(t)

	if err := p.Publish(context.Background(), factEvent()); err != nil {
		t.Fatal(err)
	}
	env, _, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(env.TraceContext, "00-") {
		t.Fatalf("envelope trace_context not a W3C traceparent: %q", env.TraceContext)
	}
}

// TestTracePropagatesAcrossHop: the trace_context a producer stamps is
// extractable by the consumer side into a span context whose trace_id matches
// — proving a continuous trace across the bus hop.
func TestTracePropagatesAcrossHop(t *testing.T) {
	installTracing(t)
	p, cc := newTestProducer(t)
	if err := p.Publish(context.Background(), factEvent()); err != nil {
		t.Fatal(err)
	}
	env, _, _ := bus.Unframe(cc.sent[0].Body)

	// Re-extract via the same propagator the consumer uses.
	ctx := otel.GetTextMapPropagator().Extract(context.Background(),
		propagation.MapCarrier{"traceparent": env.TraceContext})
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		t.Fatal("stamped trace_context did not round-trip to a valid span context")
	}
	if want := traceparentTraceID(env.TraceContext); sc.TraceID().String() != want {
		t.Errorf("trace id = %s, want %s", sc.TraceID(), want)
	}
}

// TestPublishRecordsREDMetrics: a successful publish increments the ok counter
// and observes a latency sample.
func TestPublishRecordsREDMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := bus.NewBusMetrics(reg)
	cc := &captureClient{}
	p, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source: "test/inst", ProducerVersion: "v1", Tenant: "acme", Metrics: m,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Publish(context.Background(), factEvent()); err != nil {
		t.Fatal(err)
	}

	mfs, _ := reg.Gather()
	var okCount float64
	var sawLatency bool
	for _, mf := range mfs {
		switch mf.GetName() {
		case "kanz_bus_publish_total":
			for _, met := range mf.Metric {
				if labelValue(met, "result") == "ok" {
					okCount = met.GetCounter().GetValue()
				}
			}
		case "kanz_bus_publish_duration_seconds":
			if len(mf.Metric) > 0 && mf.Metric[0].GetHistogram().GetSampleCount() == 1 {
				sawLatency = true
			}
		}
	}
	if okCount != 1 {
		t.Errorf("kanz_bus_publish_total{result=ok} = %v, want 1", okCount)
	}
	if !sawLatency {
		t.Error("kanz_bus_publish_duration_seconds missing a sample")
	}
}

// TestNilMetricsSafe: a Producer with no metrics wired must not panic.
func TestNilMetricsSafe(t *testing.T) {
	p, _ := newTestProducer(t) // no Metrics in config
	if err := p.Publish(context.Background(), factEvent()); err != nil {
		t.Fatal(err)
	}
}

func traceparentTraceID(tp string) string {
	parts := strings.Split(tp, "-")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

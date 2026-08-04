package bus_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/eighred/kanz/pkg/bus"
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

// counterByLabels sums a counter family's samples whose labels all match want.
// The second return distinguishes "the series exists" from "no series was
// exported at all", which is the distinction
// TestDLQParkIsCountedOnlyWhenTheParkSucceeded turns on — a CounterVec with no
// WithLabelValues call exports nothing, and `absent` and `zero` are different
// claims about whether a message was parked.
func counterByLabels(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) (float64, bool) {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	var present bool
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, met := range mf.Metric {
			match := true
			for k, v := range want {
				if labelValue(met, k) != v {
					match = false
					break
				}
			}
			if match {
				present = true
				total += met.GetCounter().GetValue()
			}
		}
	}
	return total, present
}

// TestDLQParkIsCountedByClass: a park counts once, against the ORIGINAL
// subject, the consuming group, and the class the parked message carries in
// Kanz-DLQ-Class. The class split is the point — a redrive returns a transient
// park to its subject and REFUSES a terminal one, so an operator reading the
// series has to be able to tell which incident they are in without opening the
// DLQ.
func TestDLQParkIsCountedByClass(t *testing.T) {
	t.Run("handler failure is transient", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		m := bus.NewBusMetrics(reg)
		sub := &oneShotSub{msg: bus.Message{Body: frame(t, validEnvelope(), nil)}}
		c, _ := bus.NewConsumer(sub, fastRetry(1), bus.WithDLQ(&captureClient{}), bus.WithBusMetrics(m))

		fh := &flakyHandler{failures: 999}
		if err := c.Subscribe(context.Background(), "market.equity.trade", "g", fh.handle); err != nil {
			t.Fatalf("expected the delivery to be parked and acked, got %v", err)
		}

		got, ok := counterByLabels(t, reg, "kanz_bus_dlq_parked_total", map[string]string{
			"subject": "market.equity.trade", "group": "g", "class": bus.ClassTransient,
		})
		if !ok || got != 1 {
			t.Errorf("kanz_bus_dlq_parked_total{subject=market.equity.trade,group=g,class=transient} = %v (present=%v), want 1",
				got, ok)
		}
	})

	t.Run("unframeable bytes are terminal", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		m := bus.NewBusMetrics(reg)
		sub := &oneShotSub{msg: bus.Message{Body: []byte("this is not an envelope frame")}}
		c, _ := bus.NewConsumer(sub, bus.WithDLQ(&captureClient{}), bus.WithBusMetrics(m))

		if err := c.Subscribe(context.Background(), "order.order.submit", "oms", nil); err != nil {
			t.Fatalf("expected the delivery to be parked and acked, got %v", err)
		}

		got, ok := counterByLabels(t, reg, "kanz_bus_dlq_parked_total", map[string]string{
			"subject": "order.order.submit", "group": "oms", "class": bus.ClassTerminal,
		})
		if !ok || got != 1 {
			t.Errorf("kanz_bus_dlq_parked_total{...,class=terminal} = %v (present=%v), want 1", got, ok)
		}
	})
}

// TestDLQParkIsCountedOnlyWhenTheParkSucceeded is the reason this metric is not
// kanz_bus_consume_total{result="error"} under another name.
//
// When the DLQ publish fails, the dispatch failed AND nothing was parked: on
// Kafka the subscription halts holding the offset, on NATS the broker
// redelivers. consume_total{result="error"} moves in both cases and cannot tell
// them apart. If kanz_bus_dlq_parked_total moved here too it would report a
// recoverable copy on dlq.<subject> that was never written, and send whoever
// read it to kanz-redrive to look for a message that is not there.
func TestDLQParkIsCountedOnlyWhenTheParkSucceeded(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := bus.NewBusMetrics(reg)
	sub := &oneShotSub{msg: bus.Message{Body: frame(t, validEnvelope(), nil)}}
	c, _ := bus.NewConsumer(sub, fastRetry(1),
		bus.WithDLQ(&errPublisher{err: errors.New("dlq down")}), bus.WithBusMetrics(m))

	fh := &flakyHandler{failures: 999}
	if err := c.Subscribe(context.Background(), "order.order.submit", "oms", fh.handle); err == nil {
		t.Fatal("expected the failed park to surface as an error")
	}

	if _, ok := counterByLabels(t, reg, "kanz_bus_dlq_parked_total", nil); ok {
		t.Error("kanz_bus_dlq_parked_total exported a series for a park that FAILED — " +
			"the message is not on dlq.order.order.submit and no redrive will find it")
	}
	// The dispatch failure itself is still counted, which is the series that
	// conflates the two outcomes and the reason the park needs its own.
	got, ok := counterByLabels(t, reg, "kanz_bus_consume_total", map[string]string{"result": "error"})
	if !ok || got != 1 {
		t.Errorf("kanz_bus_consume_total{result=error} = %v (present=%v), want 1", got, ok)
	}
}

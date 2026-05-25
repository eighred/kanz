package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func newProvider(t *testing.T, buf *bytes.Buffer) *Provider {
	t.Helper()
	p, err := New(context.Background(), Config{ServiceName: "test-svc", ServiceVersion: "v0", SampleRatio: 1},
		slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

func TestTraceHandlerInjectsSpanContext(t *testing.T) {
	var buf bytes.Buffer
	p := newProvider(t, &buf)

	ctx, span := p.Tracer.Start(context.Background(), "op")
	wantTrace := span.SpanContext().TraceID().String()
	wantSpan := span.SpanContext().SpanID().String()
	p.Logger.InfoContext(ctx, "with span")
	span.End()

	p.Logger.Info("without span")

	lines := splitLogs(t, &buf)
	if len(lines) != 2 {
		t.Fatalf("want 2 log lines, got %d", len(lines))
	}
	if lines[0]["trace_id"] != wantTrace || lines[0]["span_id"] != wantSpan {
		t.Errorf("span-bearing log missing/wrong ids: trace=%v span=%v", lines[0]["trace_id"], lines[0]["span_id"])
	}
	if _, ok := lines[1]["trace_id"]; ok {
		t.Errorf("no-span log should not carry trace_id, got %v", lines[1]["trace_id"])
	}
}

// WithAttrs must preserve the wrapper, or trace injection silently stops the
// moment a caller does logger.With(...) — the common path, and one where
// trace_id stays a top-level field (no group nesting).
func TestTraceHandlerSurvivesWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	p := newProvider(t, &buf)

	ctx, span := p.Tracer.Start(context.Background(), "op")
	want := span.SpanContext().TraceID().String()
	p.Logger.With("k", "v").InfoContext(ctx, "msg")
	span.End()

	lines := splitLogs(t, &buf)
	if lines[0]["trace_id"] != want {
		t.Errorf("trace_id lost through With: got %v want %v", lines[0]["trace_id"], want)
	}
	if lines[0]["k"] != "v" {
		t.Errorf("With attr lost: %v", lines[0]["k"])
	}
}

func TestMetricsHandlerServesBaselineAndRegistered(t *testing.T) {
	var buf bytes.Buffer
	p := newProvider(t, &buf)

	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "kanz_test_custom_total", Help: "h"})
	p.Registry.MustRegister(c)
	c.Inc()

	rr := httptest.NewRecorder()
	p.MetricsHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"go_goroutines", "process_", "kanz_test_custom_total"} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}

// The foundation must install the W3C propagator so OBS-01d bus hops can
// inject/extract trace context off the envelope.
func TestPropagatorInstalled(t *testing.T) {
	var buf bytes.Buffer
	_ = newProvider(t, &buf)

	carrier := propagation.MapCarrier{
		"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
	}
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		t.Fatal("propagator did not extract a valid span context from traceparent")
	}
	if got := sc.TraceID().String(); got != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("trace id = %s", got)
	}
}

func splitLogs(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("bad log json %q: %v", ln, err)
		}
		out = append(out, m)
	}
	return out
}

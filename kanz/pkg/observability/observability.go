// Package observability is the kanz telemetry foundation (OBS-01a): one
// Provider that bundles a Prometheus metric registry, an OTel tracer, and a
// trace-correlated slog logger, plus the /metrics HTTP handler.
//
// Metrics go through the Prometheus client_golang registry directly rather
// than an OTel→Prometheus exporter: the DATA-08 dashboard/alert contract
// (kanz/infra/observability) pins exact series names and label sets, and the
// otel exporter mangles names with unit/_total suffixes. The integrity (OBS-01b)
// and risk/bus (OBS-01c) exporters register their collectors on Provider.Registry
// and get the names verbatim. Tracing uses the OTel SDK so the W3C trace_context
// already on the envelope (EVT-17b) propagates across bus hops (OBS-01d); the
// slog handler stamps the active span's IDs onto every log line so logs join
// traces without a separate log pipeline.
package observability

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// Config configures the telemetry Provider.
type Config struct {
	// ServiceName + ServiceVersion identify this service on every span and on
	// the OTel resource. ServiceName is required.
	ServiceName    string
	ServiceVersion string

	// OTLPEndpoint, when non-empty (host:port), enables OTLP/gRPC span export to
	// that collector. Empty ⇒ spans are still created and trace context still
	// propagates, they are just not exported — startup never blocks on a
	// collector being reachable.
	OTLPEndpoint string

	// SampleRatio is the parent-based head-sampling ratio in [0,1]. A ratio < 0
	// is treated as 0 (sample nothing but root spans honouring parents) and a
	// ratio ≥ 1 always samples. The zero value (0.0) means "respect the parent
	// decision, never sample new roots"; callers wanting full sampling pass 1.
	SampleRatio float64
}

// Provider holds the process-wide telemetry handles. Construct one per service
// in main, defer Shutdown, and hand Registry/Logger/Tracer to the components
// that need them.
type Provider struct {
	// Registry is the Prometheus registry the metric exporters (OBS-01b/c)
	// register their collectors on and that /metrics serves.
	Registry *prometheus.Registry
	// Logger wraps the caller's base handler with trace-context injection.
	Logger *slog.Logger
	// Tracer is the service tracer; spans created from it carry the resource.
	Tracer trace.Tracer

	tp *sdktrace.TracerProvider
}

// New builds the telemetry Provider. base is the underlying slog handler the
// caller already configured (format + level); New wraps it so every log record
// emitted with a span-bearing context also carries trace_id/span_id. New also
// installs the global OTel TracerProvider and the W3C TraceContext+Baggage
// propagator so bus producers/consumers (OBS-01d) propagate spans without
// re-wiring. Call Provider.Shutdown to flush exported spans.
func New(ctx context.Context, cfg Config, base slog.Handler) (*Provider, error) {
	reg := prometheus.NewRegistry()
	// Baseline USE signals every service should expose: Go runtime + process
	// metrics (go_*, process_*) so CPU/memory/GC/fd panels work without per-
	// service wiring.
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
	))
	if err != nil {
		return nil, err
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	}
	if cfg.OTLPEndpoint != "" {
		exp, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint),
			otlptracegrpc.WithInsecure(),
		)
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return &Provider{
		Registry: reg,
		Logger:   slog.New(&traceHandler{Handler: base}),
		Tracer:   tp.Tracer(cfg.ServiceName),
		tp:       tp,
	}, nil
}

// MetricsHandler serves the Prometheus registry. Mount at GET /metrics.
func (p *Provider) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(p.Registry, promhttp.HandlerOpts{Registry: p.Registry})
}

// Shutdown flushes and stops the tracer provider (draining the batch exporter).
// Safe to call on a Provider whose tracing was never exported.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p.tp == nil {
		return nil
	}
	return p.tp.Shutdown(ctx)
}

// traceHandler decorates a slog.Handler, adding trace_id/span_id from the
// record's context when a valid span is active — the log↔trace bridge. It is a
// thin wrapper, not the OTel log SDK: kanz logs are stdout JSON shipped by the
// platform, so correlation (not a second export path) is the actual need.
type traceHandler struct {
	slog.Handler
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithGroup(name)}
}

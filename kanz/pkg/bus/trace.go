package bus

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// messagingEventType tags a span with the bus event type (the messaging
// destination, in OTel terms).
func messagingEventType(eventType string) attribute.KeyValue {
	return attribute.String("messaging.destination.name", eventType)
}

// tracer is the bus's OTel tracer. otel.Tracer is late-bound to the global
// TracerProvider, so spans are no-ops until observability.New installs a real
// provider — the bus never needs a tracer injected (OBS-01d).
var tracer = otel.Tracer("github.com/kanz-eng/kanz/pkg/bus")

// startProducerSpan opens a PRODUCER span around a publish. The span is active
// on the returned ctx, so the producer's trace_context stamping
// (observability.TraceparentFromContext) captures THIS span — that is what the
// consuming hop continues.
func startProducerSpan(ctx context.Context, eventType string) (context.Context, trace.Span) {
	return tracer.Start(ctx, eventType+" publish",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(messagingEventType(eventType)),
	)
}

// startConsumerSpan opens a CONSUMER span around a delivery. The caller has
// already extracted the inbound trace_context onto ctx, so this span is a
// child of the producer's.
func startConsumerSpan(ctx context.Context, eventType string) (context.Context, trace.Span) {
	return tracer.Start(ctx, eventType+" process",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(messagingEventType(eventType)),
	)
}

// endSpan records the outcome and ends the span. A nil err leaves the span
// unset (OK); a non-nil err is recorded and the span marked Error.
func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

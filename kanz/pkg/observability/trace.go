package observability

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// traceparentKey is the one W3C field the envelope carries as trace_context
// (EVT-17b). The bus moves a single string per hop, so the propagation
// carrier is single-field — no tracestate channel today.
const traceparentKey = "traceparent"

// TraceparentFromContext serializes the span active in ctx to a W3C
// traceparent string suitable for the envelope's trace_context field. It
// returns "" when no span is active. Producers stamp the result so the next
// bus hop (OBS-01d) can continue the same trace. Uses the global propagator
// installed by New (a no-op until then, returning "").
func TraceparentFromContext(ctx context.Context) string {
	c := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, c)
	return c[traceparentKey]
}

// ContextWithTraceparent extracts an inbound traceparent (an envelope's
// trace_context) into ctx as the remote span context, so a span started from
// the returned ctx is a child of the producer's span and logs emitted under it
// carry the shared trace_id. An empty traceparent returns ctx unchanged.
func ContextWithTraceparent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{traceparentKey: traceparent})
}

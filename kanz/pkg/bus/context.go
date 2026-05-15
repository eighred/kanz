package bus

import "context"

// Context-key helpers carry envelope lineage across handler boundaries so a
// downstream Producer.Publish auto-inherits propagation fields. Consumer
// (consumer.go) stashes the inbound envelope's correlation_id, the inbound
// event_id (as the *next* event's causation_id), and the inbound
// trace_context onto the handler's ctx. Producer.Publish reads them when the
// corresponding Event field is empty — caller-explicit values always win.

type ctxKey int

const (
	correlationCtxKey ctxKey = iota
	causationCtxKey
	traceCtxKey
)

func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationCtxKey, id)
}

func CorrelationIDFromContext(ctx context.Context) string {
	s, _ := ctx.Value(correlationCtxKey).(string)
	return s
}

func WithCausationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, causationCtxKey, id)
}

func CausationIDFromContext(ctx context.Context) string {
	s, _ := ctx.Value(causationCtxKey).(string)
	return s
}

func WithTraceContext(ctx context.Context, tc string) context.Context {
	return context.WithValue(ctx, traceCtxKey, tc)
}

func TraceContextFromContext(ctx context.Context) string {
	s, _ := ctx.Value(traceCtxKey).(string)
	return s
}

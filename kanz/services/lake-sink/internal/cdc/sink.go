// Package cdc folds the Kafka event log into the lakehouse: each delivered event
// becomes a Row landed via the Sink (LAKE-01a). It is the bus consumer half of
// the CDC sink — the schema-evolution-aware decode lives in package decode, the
// landing format in package sink.
package cdc

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/services/lake-sink/internal/decode"
	"github.com/eighred/kanz/services/lake-sink/internal/sink"
)

// EventSink materializes events into the lakehouse Sink. Its Handle method
// matches bus.EventHandler, so it subscribes to the durable-log topics directly.
type EventSink struct {
	decoder *decode.Decoder
	sink    sink.Sink
	now     func() time.Time
	logger  *slog.Logger
	metrics *Metrics
}

func NewEventSink(decoder *decode.Decoder, s sink.Sink, now func() time.Time, logger *slog.Logger, m *Metrics) *EventSink {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &EventSink{decoder: decoder, sink: s, now: now, logger: logger, metrics: m}
}

// Handle lands one event. Decode outcomes:
//   - ok: the row carries the decoded payload as a nested object.
//   - transient resolve failure (registry down): return the error so the bus
//     redelivers — never drop the chance to land the decoded payload.
//   - permanent decode failure (unknown ref, malformed payload): land the row
//     envelope-only with DecodeError set and ack, so a poison message neither
//     blocks the partition nor erases the fact that the event occurred.
//
// A Sink write failure is returned so the bus retries; the lake sees
// at-least-once delivery (duplicates share event_id, compacted downstream).
//
// Handle does not return nil until the row it landed is durable. The bus
// (pkg/bus/kafka.go) commits the Kafka offset the instant Handle returns
// nil — CommitInterval is 0, so that commit is synchronous and irreversible.
// A row sitting only in FileSink's bufio buffer is process-local memory: on
// a crash it is gone, but its offset is already committed, so the consumer
// group resumes past it forever. Every path below that lands a row therefore
// flushes (bufio.Flush + fsync, via Sink.Flush) before returning nil. A flush
// failure is treated as transient — the error is returned so the bus does
// not commit and the broker redelivers, which is safe because a duplicate
// write is resolved by downstream compaction (see the Sink doc comment).
func (e *EventSink) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	ref := env.GetPayloadSchemaRef()
	domain := env.GetDomain()
	entity := entityFrom(ref, env.GetEventType())

	payloadJSON, decErr := e.decoder.Decode(ctx, ref, payload)
	if decErr != nil {
		var te *decode.TransientError
		if errors.As(decErr, &te) {
			e.metrics.incDecode("transient")
			return decErr
		}
		e.metrics.incDecode("permanent")
		e.logger.WarnContext(ctx, "payload decode failed; landing envelope-only",
			"ref", ref, "event_id", env.GetEventId(), "err", decErr)
	}

	row := sink.Row{
		EventID:       env.GetEventId(),
		CorrelationID: env.GetCorrelationId(),
		CausationID:   env.GetCausationId(),
		Domain:        domain,
		Entity:        entity,
		EventType:     env.GetEventType(),
		EventClass:    className(env.GetEventClass()),
		TenantID:      env.GetTenantId(),
		Source:        env.GetSource(),
		PartitionKey:  env.GetPartitionKey(),
		EventTime:     env.GetEventTime().AsTime(),
		SchemaVersion: env.GetSchemaVersion(),
		SchemaRef:     ref,
		IngestedAt:    e.now().UTC(),
		Payload:       payloadJSON,
	}
	if decErr != nil {
		row.DecodeError = decErr.Error()
	}

	if err := e.sink.Write(ctx, row); err != nil {
		e.metrics.incRow(domain, entity, "error")
		return err
	}
	// The row is buffered but not yet durable. Flush before acking — this is
	// the barrier that keeps the offset commit from racing ahead of the row
	// actually reaching disk (see the Handle doc comment above). Covers both
	// the decoded-ok row and the permanent-decode-error row: both reach here.
	if err := e.sink.Flush(); err != nil {
		e.metrics.incRow(domain, entity, "error")
		return err
	}
	e.metrics.incRow(domain, entity, "ok")
	return nil
}

// entityFrom derives the table's entity from the payload schema ref's message
// name ("market.v1.MarketDataEvent:7" → "MarketDataEvent"), falling back to the
// envelope event_type for envelope-only events.
func entityFrom(ref, eventType string) string {
	if ref != "" {
		id := ref
		if i := strings.LastIndex(ref, ":"); i >= 0 {
			id = ref[:i]
		}
		if j := strings.LastIndex(id, "."); j >= 0 {
			return id[j+1:]
		}
		return id
	}
	if eventType != "" {
		return eventType
	}
	return "unknown"
}

func className(c envelopepb.EventClass) string {
	return strings.TrimPrefix(c.String(), "EVENT_CLASS_")
}

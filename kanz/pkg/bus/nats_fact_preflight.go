package bus

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"
)

// ErrPublishFrameTooLarge means the FACT's bounded frame and transport headers
// exceed the server's negotiated maximum payload.
var ErrPublishFrameTooLarge = errors.New("bus: NATS publication exceeds maximum payload")

// ErrPublishCapacityUnknown means a NATS connection has no current advertised
// capacity. A caller can retry once the connection is established.
var ErrPublishCapacityUnknown = errors.New("bus: NATS publication capacity unavailable")

// PreflightNATSFact checks a durable FACT against this connection's advertised
// MaxPayload before a caller commits its outbox record. It uses the record's
// lineage and this producer's identity. No broker request or sequence allocation
// is needed. Other transports keep their existing publication behavior.
// Stream retention, availability and acknowledgement remain publish
// time concerns. A later change to producer identity or broker configuration
// requires the corresponding limits to be qualified again.
func (p *Producer) PreflightNATSFact(ctx context.Context, e Event) error {
	c, ok := p.client.(*NATSClient)
	if !ok {
		return nil
	}
	if c == nil || c.conn == nil || !c.conn.IsConnected() {
		return ErrPublishCapacityUnknown
	}
	limit := c.conn.MaxPayload()
	if limit <= 0 {
		return ErrPublishCapacityUnknown
	}
	if e.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT || e.Payload == nil {
		return errors.New("bus: NATS FACT preflight requires a FACT with a payload")
	}
	if e.EventID == "" {
		if e.IdempotencyKey != "" {
			return errors.New("bus: an explicit FACT idempotency key requires a persisted event_id")
		}
		// The real UUIDv7 will occupy the same bytes in the envelope and headers.
		e.EventID = "00000000-0000-7000-8000-000000000000"
	}
	// A negative second and five-byte nanoseconds reserve the largest valid
	// Timestamp encoding. A retry can also have a wider producer sequence.
	env, err := p.stamp(ctx, e, time.Unix(-1, 999999999).UTC(), func(_, partition string) uint64 {
		if partition == "" {
			return 0
		}
		return math.MaxUint64
	})
	if err != nil {
		return err
	}
	if e.TraceContext == "" {
		// Publish starts its own span. The configured W3C propagator can
		// supply a traceparent even if the caller had no active span.
		const traceparent = "00-00000000000000000000000000000001-0000000000000001-01"
		if len(env.TraceContext) < len(traceparent) {
			env.TraceContext = traceparent
		}
	}
	if err := Validate(env); err != nil {
		return fmt.Errorf("envelope validation: %w", err)
	}
	payload, err := proto.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("payload marshal: %w", err)
	}
	msg, err := framedMessage(e, env, payload)
	if err != nil {
		return err
	}
	wire := natsMessage(msg)
	// nats.Msg.Size includes routing fields. Conn.MaxPayload counts only the
	// data plus encoded headers, including the partition and deduplication keys.
	size := int64(wire.Size() - len(wire.Subject) - len(wire.Reply))
	if size > limit {
		return fmt.Errorf("%w: bounded size %d, limit %d", ErrPublishFrameTooLarge, size, limit)
	}
	return nil
}

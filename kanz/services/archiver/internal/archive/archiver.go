// Package archive is the NATS→Kafka archiver (DATA-M1): the producer that makes
// Kafka the durable log of record it was always documented to be.
//
// Before this existed, bus.DialKafka had exactly ONE non-test caller in the
// repository — lake-sink, and it SUBSCRIBES. Nothing had ever written an event to
// Kafka. The EXECUTION stream has a 24h max-age, so the platform's history —
// realized P&L since inception, the audit trail, every fill older than yesterday —
// was retained nowhere at all.
//
// Two invariants hold this together, and both are about the same thing:
//
//   - ACK ONLY AFTER KAFKA ACKNOWLEDGES. Every failure returns an error, which
//     NACKs, which redelivers. Acking an event we failed to archive is silent,
//     permanent loss. That makes delivery AT-LEAST-ONCE: a redelivery after a
//     successful produce whose ack was lost writes a duplicate, and event_id (on
//     the Kanz-Event-Id header) is the dedup key downstream. Duplicates are
//     recoverable; a hole is not.
//
//   - THE ENVELOPE IS ARCHIVED VERBATIM. The archiver never re-stamps, re-validates
//     or re-serializes. It is a transport, not a producer — a component that
//     rewrites the log of record on its way into the log of record cannot be trusted
//     as a record of what happened. It unframes ONLY to read routing fields.
//
// SINGLE WRITER, DELIBERATELY. One replica per stream (Recreate, not RollingUpdate).
// Two producers can invert per-key order in Kafka, and a reordered log rebuilds a
// DIFFERENT book downstream — a subtler failure than losing it. A gap here is not
// loss the way it was for webhook-ingest (EXEC-M22): NATS retains 24h (168h on the
// money streams) and a restarted pod catches up. The real failure mode is FALLING
// BEHIND, which is why lag is a first-class metric.
package archive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/services/archiver/internal/topic"
)

// HeaderEventID carries the envelope's event_id so a downstream consumer can dedup
// an at-least-once redelivery WITHOUT parsing the payload.
const HeaderEventID = "Kanz-Event-Id"

// Publisher is the Kafka side. Satisfied by *bus.KafkaClient.
type Publisher interface {
	Publish(ctx context.Context, msg bus.Message) error
}

// Subscriber is the NATS side. Satisfied by *bus.NATSClient. A handler returning a
// non-nil error NACKs the message.
type Subscriber interface {
	Subscribe(ctx context.Context, subject, group string, h bus.Handler) error
}

// Config configures an Archiver.
type Config struct {
	// Tenant is the tenant this archiver serves (ARCHIVER_TENANT). An envelope
	// carrying any other tenant_id is refused, not misfiled.
	Tenant string
	// Group is the durable consumer name.
	Group string
	// Subjects are the NATS subject patterns to archive (one durable per subject).
	Subjects []string
	Kafka    Publisher
	NATS     Subscriber
	Logger   *slog.Logger
	// Ready, if set, is invoked once every subject's Subscribe goroutine has been
	// launched — NOT once messages are flowing, since Subscribe itself doesn't
	// signal that. It lets a caller (main) flip readiness only after subscriptions
	// are actually being established, instead of at construction time.
	Ready func()
	// Metrics is added in Task 6. Leave it out for now.
}

// Archiver drains NATS subjects into Kafka topics.
type Archiver struct {
	cfg Config
}

// New builds an Archiver.
func New(cfg Config) *Archiver { return &Archiver{cfg: cfg} }

// Run subscribes every configured subject CONCURRENTLY and blocks until ctx is
// done or a subscription fails.
//
// bus.NATSClient.Subscribe does not return until ctx is done — it is a blocking,
// per-subject call by design (one JetStream consumer per subject). A sequential
// loop over cfg.Subjects therefore blocks forever inside its first iteration,
// leaving every subject after the first NEVER subscribed for the life of the
// process while the service reports healthy. Each subject gets its own
// goroutine instead — mirroring lake-sink's runSink (services/lake-sink/cmd/lake-sink/main.go).
//
// The first non-cancellation error cancels the remaining subscriptions and is
// returned: a broken subscription must bring the archiver down, not silently
// archive fourteen of fifteen streams.
func (a *Archiver) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for _, subject := range a.cfg.Subjects {
		wg.Add(1)
		go func(subject string) {
			defer wg.Done()
			a.cfg.Logger.Info("archiving", "subject", subject, "group", a.cfg.Group, "tenant", a.cfg.Tenant)
			if err := a.cfg.NATS.Subscribe(ctx, subject, a.cfg.Group, a.Handle); err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = fmt.Errorf("archiver: subscribe %q: %w", subject, err)
					cancel()
				})
			}
		}(subject)
	}
	if a.cfg.Ready != nil {
		a.cfg.Ready()
	}
	wg.Wait()
	return firstErr
}

// Handle archives one message. A non-nil return NACKs it — which is the point: the
// event stays in NATS until it is safely in Kafka.
func (a *Archiver) Handle(ctx context.Context, msg bus.Message) error {
	env, _, err := bus.Unframe(msg.Body)
	if err != nil {
		// Not decodable ⇒ we cannot even name it. NACK; do not drop.
		a.cfg.Logger.Error("archiver: undecodable envelope", "subject", msg.Subject, "err", err)
		return fmt.Errorf("archiver: unframe: %w", err)
	}

	name, err := topic.For(env, a.cfg.Tenant)
	if err != nil {
		a.cfg.Logger.Error("archiver: refusing to route event",
			"subject", msg.Subject, "event_id", env.GetEventId(), "event_type", env.GetEventType(), "err", err)
		return fmt.Errorf("archiver: topic: %w", err)
	}

	// Body VERBATIM. Key = partition_key, so events sharing a key land on one
	// partition, in order.
	out := bus.Message{
		Subject: name,
		Key:     []byte(env.GetPartitionKey()),
		Body:    msg.Body,
		Headers: map[string]string{HeaderEventID: env.GetEventId()},
	}
	if err := a.cfg.Kafka.Publish(ctx, out); err != nil {
		// NACK. Never ack an event we failed to archive.
		a.cfg.Logger.Error("archiver: kafka produce failed — NACKing",
			"topic", name, "event_id", env.GetEventId(), "err", err)
		return fmt.Errorf("archiver: publish to %q: %w", name, err)
	}
	return nil
}

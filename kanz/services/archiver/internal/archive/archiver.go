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
//   - ACK ONLY AFTER THE EVENT IS SAFE — either in its real Kafka topic or in the
//     DLQ. A TRANSIENT failure (Kafka unreachable, timeout) returns an error, which
//     NACKs and redelivers: retrying is the only way it ever succeeds. A TERMINAL
//     failure (an envelope that will never decode or never map to a topic) is
//     produced verbatim to dlq.archiver and then ACKED — NACK-looping an event that
//     can never succeed just burns the delivery loop until a human purges the
//     stream. See Handle and deadLetter. Acking an event that reached NEITHER its
//     topic NOR the DLQ is silent, permanent loss and must never happen: if the DLQ
//     produce itself fails, that failure is transient, so it NACKs too. That makes
//     delivery AT-LEAST-ONCE: a redelivery after a successful produce whose ack was
//     lost writes a duplicate, and event_id (on the Kanz-Event-Id header) is the
//     dedup key downstream. Duplicates are recoverable; a hole is not.
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
	"time"

	"github.com/eighred/kanz/internal/topic"
	"github.com/eighred/kanz/pkg/bus"
)

// HeaderEventID carries the envelope's event_id so a downstream consumer can dedup
// an at-least-once redelivery WITHOUT parsing the payload.
const HeaderEventID = "Kanz-Event-Id"

// DLQSubject is the single sink for events Handle can never route: it is
// un-prefixed (not tenant.domain.entity) because dlq is a reserved leading
// segment (see internal/topic) and an unmappable event has no {domain}.{entity}
// to derive a per-topic DLQ name from in the first place.
const DLQSubject = "dlq.archiver"

// Why Handle did not land an event on its real topic. ONE VOCABULARY, TWO
// SURFACES: these are the "reason" label on kanz_archiver_failed_total (see
// NewMetrics) and the prefix of the parked message's bus.HeaderDLQError, so the
// metric an operator alerts on and the header they read next agree by
// construction rather than by care.
//
// They used to ALSO be a wire header of this package's own, `Kanz-DLQ-Reason`,
// declared beside hand-rolled `Kanz-DLQ-Subject` and `Kanz-DLQ-Error` constants
// — three spellings of pkg/bus's DLQ contract, one of them (`Kanz-DLQ-Subject`
// vs `Kanz-DLQ-Original-Subject`) a near-miss of the single field a drain routes
// on. #285 removed all three. The header is gone rather than renamed because
// both of its values are bus.ClassTerminal, so it answered the same question
// bus.HeaderDLQClass answers, one level finer — see deadLetter.
const (
	reasonUnframe    = "unframe"
	reasonRoute      = "route"
	reasonPublish    = "publish"
	reasonDLQPublish = "dlq_publish"
)

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
	// Metrics records outcome counters (Archived/Failed) and is consulted by
	// Handle on every exit path. Optional — nil is a no-op, so a Config built
	// without it (existing tests, callers that haven't wired observability yet)
	// behaves exactly as before Task 6.
	Metrics *Metrics
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

// Handle archives one message.
//
// A non-nil return NACKs — reserved for TRANSIENT failures, where retry is the
// only path to success: the real-topic Kafka produce. A nil return on a TERMINAL
// failure (undecodable body, unroutable event) means the event was dead-lettered
// to DLQSubject, not that it was silently dropped — see deadLetter.
func (a *Archiver) Handle(ctx context.Context, msg bus.Message) error {
	env, _, err := bus.Unframe(msg.Body)
	if err != nil {
		// Not decodable ⇒ it will NEVER decode differently on redelivery. Terminal.
		a.cfg.Logger.Error("archiver: undecodable envelope", "subject", msg.Subject, "err", err)
		a.observeFail("", reasonUnframe)
		return a.deadLetter(ctx, msg, reasonUnframe, err, "")
	}

	name, err := topic.For(env, a.cfg.Tenant)
	if err != nil {
		// Malformed event_type, cross-tenant, reserved prefix ⇒ it will NEVER map
		// on redelivery. Terminal.
		a.cfg.Logger.Error("archiver: refusing to route event",
			"subject", msg.Subject, "event_id", env.GetEventId(), "event_type", env.GetEventType(), "err", err)
		a.observeFail("", reasonRoute)
		return a.deadLetter(ctx, msg, reasonRoute, err, env.GetEventId())
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
		// TRANSIENT — broker down, timeout — succeeds on retry. NACK. Never ack an
		// event we failed to archive.
		a.cfg.Logger.Error("archiver: kafka produce failed — NACKing",
			"topic", name, "event_id", env.GetEventId(), "err", err)
		a.observeFail(name, reasonPublish)
		return fmt.Errorf("archiver: publish to %q: %w", name, err)
	}
	if a.cfg.Metrics != nil {
		a.cfg.Metrics.Archived.WithLabelValues(name).Inc()
	}
	return nil
}

// observeFail records a Failed outcome. topic may be "" (unframe/route fail
// before a topic is known). Metrics is optional; nil is a no-op.
func (a *Archiver) observeFail(topic, reason string) {
	if a.cfg.Metrics != nil {
		a.cfg.Metrics.Failed.WithLabelValues(topic, reason).Inc()
	}
}

// deadLetter produces msg's raw body VERBATIM to DLQSubject with headers naming
// the cause, then returns nil so the caller ACKs: a terminal failure will never
// succeed on redelivery, so retrying it only burns the delivery loop.
//
// If the DLQ produce itself fails, that is ordinary Kafka unavailability —
// TRANSIENT — so this returns a non-nil error (NACK) instead. An event must never
// be acked having reached neither its real topic nor the DLQ; "terminal" describes
// the routing decision, not a license to drop the event on a second failure.
//
// THE HEADERS ARE pkg/bus's, NOT THIS PACKAGE'S (#285). The DLQ header names are
// one wire contract with one home, and this package had grown its own near-miss
// spelling of the field a drain routes on — `Kanz-DLQ-Subject` where pkg/bus
// writes `Kanz-DLQ-Original-Subject`. Nothing caught it because until #220 built
// the drain, nothing read either name. test/arch/dlq_header_test.go now fails the
// build on a `Kanz-DLQ-*` literal outside pkg/bus.
//
// BOTH PARK REASONS ARE ClassTerminal, and that is a decision, not a default.
// Every path into here is a pure function of the bytes and this archiver's static
// config: bus.Unframe on a body that will never decode, or topic.For on an
// event_type/tenant that will never map. Nothing in the WORLD changes to make
// either succeed — which is exactly pkg/bus's terminal/transient line, and the
// same call metrics.go's "reason" documentation already makes. Marking `route`
// transient because a routing table could be corrected would invite an automatic
// drain to replay it immediately, fail identically and re-park: the loop the class
// exists to prevent. Recovery after a config fix is what --include-terminal is
// for.
//
// NO DRAIN READS THIS TOPIC, AND THAT IS THE STANDING GAP (#285 item 3).
// cmd/kanz-redrive is NATS-only; dlq.archiver is Kafka, has no consumer, and
// infra/kafka/topics-job.yaml deletes it after 30 days. The headers below are what
// a drain needs — where to send it back (bus.HeaderDLQOriginalSubject), how old it
// is (bus.HeaderDLQParkedAt, the min-age gate) and whether it may be replayed at
// all (bus.HeaderDLQClass) — so that drain does not have to guess. Until it
// exists, recovering a parked event means an operator reading the topic by hand.
//
// bus.HeaderDLQAttempts is deliberately ABSENT rather than fabricated: bus.Message
// carries no delivery counter, so this cannot know whether NATS redelivered the
// event once or ten times, and a hardcoded "1" would read as fact. It is
// diagnostic only — no redrive decision reads it. bus.HeaderDLQRedrives is absent
// for the same honesty: nothing redrives these yet, and redriveCount already reads
// an absent header as zero.
func (a *Archiver) deadLetter(ctx context.Context, msg bus.Message, reason string, cause error, eventID string) error {
	headers := map[string]string{
		bus.HeaderDLQOriginalSubject: msg.Subject,
		// The reason prefix is what `Kanz-DLQ-Reason` used to carry. It rides in
		// the error rather than a seventh header because two fields answering
		// "why did this park" is the divergence this change removed, and the
		// machine-readable copy already exists as the metric's reason label.
		bus.HeaderDLQError:    reason + ": " + cause.Error(),
		bus.HeaderDLQParkedAt: time.Now().UTC().Format(time.RFC3339Nano),
		bus.HeaderDLQClass:    bus.ClassTerminal,
	}
	if eventID != "" {
		headers[HeaderEventID] = eventID
	}
	out := bus.Message{
		Subject: DLQSubject,
		Body:    msg.Body,
		Headers: headers,
	}
	if err := a.cfg.Kafka.Publish(ctx, out); err != nil {
		a.cfg.Logger.Error("archiver: dlq produce failed — NACKing",
			"reason", reason, "cause", cause, "err", err)
		// The underlying cause (reason: "unframe"/"route") was already counted by
		// the caller — this is the SEPARATE, more severe outcome: the event
		// reached neither its real topic nor the DLQ.
		a.observeFail(DLQSubject, reasonDLQPublish)
		return fmt.Errorf("archiver: dlq publish: %w", err)
	}
	a.cfg.Logger.Warn("archiver: terminal failure — dead-lettered and acked",
		"reason", reason, "subject", msg.Subject, "cause", cause)
	return nil
}

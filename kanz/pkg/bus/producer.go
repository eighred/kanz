package bus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
)

// envelopeVersion is the Envelope schema version this client builds against.
// Bumps once per additive change to envelope.proto (rare; see
// kanz-schemas/docs/envelope-policy.md §6).
const envelopeVersion uint32 = 1

// Event is the producer-facing form: caller-known envelope fields plus the
// domain payload. Auto fields (event_id, publish_time, source,
// producer_version, producer_sequence, envelope_version, correlation_id for
// roots, idempotency_key for non-COMMAND classes) are stamped by Publish.
type Event struct {
	Subject string

	EventType        string
	EventClass       envelopepb.EventClass
	SchemaVersion    uint32
	Domain           string
	EventTime        time.Time
	IngestionTime    time.Time // zero ⇒ stamped to now()
	CorrelationID    string    // empty on roots; Producer fills with event_id
	CausationID      string
	TraceContext     string
	PartitionKey     string
	IdempotencyKey   string // required for COMMAND; must be empty/== event_id otherwise
	PayloadSchemaRef string
	QualityFlags     []envelopepb.QualityFlag

	Payload proto.Message
}

// ProducerConfig pins the identity fields the producer stamps on every event.
type ProducerConfig struct {
	Source          string // service/instance, e.g. "market-ingest/pod-7"
	ProducerVersion string // git SHA or semver
}

// Producer stamps + validates envelopes and frames them onto a Client.
// Goroutine-safe; the per-(event_type, partition_key) sequence counter is
// guarded by a mutex.
type Producer struct {
	client Client
	cfg    ProducerConfig

	seqMu    sync.Mutex
	sequence map[seqKey]uint64
}

type seqKey struct {
	eventType    string
	partitionKey string
}

func NewProducer(client Client, cfg ProducerConfig) (*Producer, error) {
	if client == nil {
		return nil, errors.New("bus: client is nil")
	}
	if cfg.Source == "" {
		return nil, errors.New("bus: ProducerConfig.Source required")
	}
	if cfg.ProducerVersion == "" {
		return nil, errors.New("bus: ProducerConfig.ProducerVersion required")
	}
	return &Producer{
		client:   client,
		cfg:      cfg,
		sequence: make(map[seqKey]uint64),
	}, nil
}

// Publish stamps the auto-fields onto the envelope, validates the result,
// frames envelope+payload, and hands the wire bytes to the underlying Client.
func (p *Producer) Publish(ctx context.Context, e Event) error {
	if e.Payload == nil {
		return errors.New("bus: Event.Payload required")
	}
	env, err := p.stamp(ctx, e)
	if err != nil {
		return err
	}
	if err := Validate(env); err != nil {
		return fmt.Errorf("envelope validation: %w", err)
	}
	payloadBytes, err := proto.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("payload marshal: %w", err)
	}
	body, err := proto.Marshal(&envelopepb.EventFrame{
		Envelope: env,
		Payload:  payloadBytes,
	})
	if err != nil {
		return fmt.Errorf("frame marshal: %w", err)
	}
	return p.client.Publish(ctx, Message{
		Subject: e.Subject,
		Key:     []byte(env.PartitionKey),
		Body:    body,
		// NATS JetStream keys its broker-side dedup window on Nats-Msg-Id
		// (EVT-08); Kafka treats this as an ordinary user header (harmless).
		// One header serves both transports because NATS reserves the name
		// and Kafka is name-agnostic.
		Headers: map[string]string{"Nats-Msg-Id": env.IdempotencyKey},
	})
}

func (p *Producer) stamp(ctx context.Context, e Event) (*envelopepb.Envelope, error) {
	if e.EventTime.IsZero() {
		return nil, errors.New("Event.EventTime required")
	}
	eid, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("uuid v7: %w", err)
	}
	eventID := eid.String()

	now := time.Now().UTC()
	ing := e.IngestionTime
	if ing.IsZero() {
		ing = now
	}
	// Lineage precedence: explicit Event field > ctx-derived > root default.
	// Consumer stashes inbound envelope fields onto ctx (EVT-17c) so a
	// handler that publishes a derived event auto-inherits them.
	correlation := e.CorrelationID
	if correlation == "" {
		correlation = CorrelationIDFromContext(ctx)
	}
	if correlation == "" {
		correlation = eventID
	}
	causation := e.CausationID
	if causation == "" {
		causation = CausationIDFromContext(ctx)
	}
	trace := e.TraceContext
	if trace == "" {
		trace = TraceContextFromContext(ctx)
	}

	idem := e.IdempotencyKey
	if e.EventClass == envelopepb.EventClass_EVENT_CLASS_COMMAND {
		if idem == "" {
			return nil, errors.New("idempotency_key required for COMMAND events")
		}
	} else {
		// FACT / STATE_SNAPSHOT / OBSERVATION: idempotency_key = event_id.
		if idem != "" && idem != eventID {
			return nil, errors.New("idempotency_key must equal event_id for non-COMMAND events")
		}
		idem = eventID
	}

	seq := p.nextSequence(e.EventType, e.PartitionKey)

	return &envelopepb.Envelope{
		EventId:          eventID,
		EventType:        e.EventType,
		SchemaVersion:    e.SchemaVersion,
		EnvelopeVersion:  envelopeVersion,
		EventClass:       e.EventClass,
		Domain:           e.Domain,
		EventTime:        timestamppb.New(e.EventTime.UTC()),
		IngestionTime:    timestamppb.New(ing.UTC()),
		PublishTime:      timestamppb.New(now),
		CorrelationId:    correlation,
		CausationId:      causation,
		TraceContext:     trace,
		Source:           p.cfg.Source,
		ProducerVersion:  p.cfg.ProducerVersion,
		PartitionKey:     e.PartitionKey,
		ProducerSequence: seq,
		IdempotencyKey:   idem,
		QualityFlags:     e.QualityFlags,
		PayloadSchemaRef: e.PayloadSchemaRef,
	}, nil
}

func (p *Producer) nextSequence(eventType, partitionKey string) uint64 {
	if partitionKey == "" {
		return 0 // 0 means N/A per envelope.proto.
	}
	p.seqMu.Lock()
	defer p.seqMu.Unlock()
	k := seqKey{eventType, partitionKey}
	p.sequence[k]++
	return p.sequence[k]
}

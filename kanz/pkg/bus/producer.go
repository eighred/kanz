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

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/pkg/observability"
)

// envelopeVersion is the Envelope schema version this client builds against.
// Bumps once per additive change to envelope.proto (rare; see
// kanz-schemas/docs/envelope-policy.md §6). v2 added tenant_id (MT-01a).
const envelopeVersion uint32 = 2

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
	TenantID         string // explicit tenant; empty ⇒ ctx tenant, then ProducerConfig.Tenant

	Payload proto.Message
}

// ProducerConfig pins the identity fields the producer stamps on every event.
type ProducerConfig struct {
	Source          string // service/instance, e.g. "market-ingest/pod-7"
	ProducerVersion string // git SHA or semver

	// Tenant is the producer's fallback tenant_id (MT-01b), stamped on events
	// that carry neither an explicit Event.TenantID nor a ctx tenant. A
	// single-tenant service sets it; a multi-tenant service leaves it empty and
	// supplies the tenant per-event (Event.TenantID) or via ctx (the consumer
	// stashes the inbound tenant, so derived events inherit it).
	Tenant string

	// Metrics is the optional RED exporter (OBS-01c). Nil ⇒ no instrumentation.
	Metrics *BusMetrics

	// VerifyCommandIssuer, when set, runs on every COMMAND publish: the bus
	// extracts CommandMetadata.issuer and passes it to this func to confirm the
	// caller (ctx) may issue under it (AUTH-01c forged-issuer guard). Nil ⇒ no
	// check (backward compatible); non-COMMAND events never invoke it.
	VerifyCommandIssuer CommandIssuerFunc
}

// Producer stamps + validates envelopes and frames them onto a Client.
// Goroutine-safe; the per-(event_type, partition_key) sequence counter is
// guarded by a mutex.
type Producer struct {
	client       Client
	cfg          ProducerConfig
	metrics      *BusMetrics
	verifyIssuer CommandIssuerFunc

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
		client:       client,
		cfg:          cfg,
		metrics:      cfg.Metrics,
		verifyIssuer: cfg.VerifyCommandIssuer,
		sequence:     make(map[seqKey]uint64),
	}, nil
}

// Publish opens a producer span (so the stamped trace_context continues into
// the next hop, OBS-01d), records RED metrics (OBS-01c), then stamps,
// validates, frames, and hands the wire bytes to the underlying Client.
func (p *Producer) Publish(ctx context.Context, e Event) error {
	ctx, span := startProducerSpan(ctx, e.EventType)
	start := time.Now()
	err := p.publish(ctx, e)
	endSpan(span, err)
	p.metrics.observePublish(e.Subject, time.Since(start), err)
	return err
}

func (p *Producer) publish(ctx context.Context, e Event) error {
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
	// Forged-issuer guard (AUTH-01c): a COMMAND must carry a CommandMetadata
	// issuer the caller is authorized to act under. Runs only when a verifier
	// is configured; extraction is generic over any concrete command type.
	if e.EventClass == envelopepb.EventClass_EVENT_CLASS_COMMAND && p.verifyIssuer != nil {
		issuer, err := extractCommandIssuer(payloadBytes)
		if err != nil {
			return err
		}
		if issuer == "" {
			return ErrMissingCommandIssuer
		}
		if err := p.verifyIssuer(ctx, issuer); err != nil {
			return fmt.Errorf("command issuer verification: %w", err)
		}
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
	// Trace precedence: explicit Event field > active OTel span (the producer
	// span just opened, or a span the caller started) > legacy string ctx
	// (OBS-01d; the string path stays for callers not yet span-aware).
	trace := e.TraceContext
	if trace == "" {
		trace = observability.TraceparentFromContext(ctx)
	}
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

	// Tenant precedence (MT-01b): explicit Event field > ctx-derived (the
	// consumer stashes the inbound tenant so derived events inherit it) >
	// ProducerConfig fallback. Validate rejects an empty tenant on the live path.
	tenant := e.TenantID
	if tenant == "" {
		tenant = TenantIDFromContext(ctx)
	}
	if tenant == "" {
		tenant = p.cfg.Tenant
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
		PayloadSchemaRef: schemaRef(e),
		TenantId:         tenant,
	}, nil
}

// schemaRef resolves the envelope's payload_schema_ref, which Validate REQUIRES
// on every event. An explicit Event.PayloadSchemaRef wins; otherwise it is derived
// from the payload itself.
//
// It is derived rather than demanded because it is not new information: the
// convention is exactly "{proto full name}:{schema_version}" (domain.v1.RiskMeasureSet:1,
// signal.v1.StrategySignal:1), and the producer already holds both halves — the
// payload message and e.SchemaVersion, which Validate independently requires to be
// >= 1. Asking each caller to restate it bought nothing and cost everything: TWELVE
// publish sites across the module omitted it — the whole signal→order path, every
// OMS venue connector/recon/userdata emitter, market-ingest's book snapshots, the
// gateway's order route — and each one was a publisher that validated fine in unit
// tests (which inject fake Publishers that never validate) and failed on the first
// real broker. A required field that the producer can compute itself should never
// have been the caller's job to remember.
func schemaRef(e Event) string {
	if e.PayloadSchemaRef != "" {
		return e.PayloadSchemaRef
	}
	if e.Payload == nil {
		return "" // publish() rejects a nil payload before stamp; Validate is the backstop.
	}
	return fmt.Sprintf("%s:%d", e.Payload.ProtoReflect().Descriptor().FullName(), e.SchemaVersion)
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

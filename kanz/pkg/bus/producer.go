package bus

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
// kanz-schemas/README.md § Envelope Policy §6). v2 added tenant_id (MT-01a).
const envelopeVersion uint32 = 2

// headerNatsMsgID is the header NATS JetStream keys its broker-side dedup
// window on. The name is RESERVED by the broker — a typo does not error, it
// simply stops the message being deduplicated — which is why both the write
// site here and the re-stamp in redriveMsgID spell it through this constant.
const headerNatsMsgID = "Nats-Msg-Id"

// headerExpectedLastSubjectSeq is the header JetStream reads to make a publish
// CONDITIONAL on the sequence currently last on that subject. The name is
// RESERVED by the broker; a typo does not error, it simply drops the condition
// and the publish lands unconditionally — the same silent-degradation shape as
// headerNatsMsgID, and the reason both are constants rather than literals.
//
// It exists for read-modify-write on a COMPACTED state subject: a caller that
// reads the retained message, merges into it and writes the result back must not
// clobber a value another writer put there in between. Kafka has no equivalent
// and treats this as an ordinary user header; every current user is NATS-only.
const headerExpectedLastSubjectSeq = "Nats-Expected-Last-Subject-Sequence"

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

	// ExpectedLastSubjectSeq makes this publish CONDITIONAL: the broker accepts it
	// only if the sequence given here is the last one currently on Subject, and
	// refuses it otherwise. nil ⇒ unconditional, which is what every event on an
	// append-only stream wants.
	//
	// IT IS FOR STATE SUBJECTS, WHERE THE MESSAGE IS THE WHOLE ANSWER. On a
	// compacted subject a publisher that reads the retained value, merges into it
	// and writes the result back is performing a read-modify-write, and two of
	// those racing lose one of the merges — silently, because both publishes
	// succeed. The mandate publisher is the first such caller (#916): the value it
	// writes carries the mandate in force PLUS every scheduled one, so a lost merge
	// is a mandate that vanishes off a never-aging stream.
	//
	// A pointer, not a bare uint64, because ZERO IS A REAL EXPECTATION: it asserts
	// that the subject holds no message at all, which is what the first publish for
	// a portfolio must claim. A plain uint64 could not tell "expect empty" from
	// "no expectation" and would have made exactly the first-write race unguardable.
	ExpectedLastSubjectSeq *uint64
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

	// SequenceTTL and SequenceMax bound the per-(event_type, partition_key)
	// sequence map (#805). Zero ⇒ defaultSequenceTTL / defaultSequenceMax, which
	// is what every service in this estate uses — they are here so a deployment
	// with an unusual key cardinality can say so, not because anybody must.
	//
	// See the block comment on defaultSequenceTTL for why evicting a key is safe
	// and what it costs.
	SequenceTTL time.Duration
	SequenceMax int
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
	sequence map[seqKey]seqEntry
	seqTTL   time.Duration
	seqMax   int
	// now is the sequence window's clock; nil ⇒ time.Now. A test installs one
	// to advance past the TTL without sleeping.
	now func() time.Time
}

type seqKey struct {
	eventType    string
	partitionKey string
}

// seqEntry is one key's counter and when it was last stamped.
type seqEntry struct {
	seq     uint64
	touched time.Time
}

// THE SEQUENCE MAP IS BOUNDED, AND IT WAS NOT (#805).
//
// # What it cost
//
// sequence is keyed by {event_type, partition_key} and partition_key ON THE
// ORDER PATH IS THE ORDER ID — services/oms/internal/order/events.go stamps it
// in the single builder every OMS order FACT goes through, and the api-gateway,
// both venue adapters and optimization do the same. One Producer per process,
// one entry per (event type x order id), and the only delete( in this package
// belonged to DedupWindow. So roughly 2-6 permanent entries per order the
// process had ever touched, for the life of the pod.
//
// This is the platform's highest-throughput long-lived map and it was
// monotonic. At institutional order rates the OMS heap grows until the pod is
// OOM-killed, which on the execution path means orders in flight at an unknown
// state and a restart that has to reconcile them. It arrives as a memory
// eviction rather than an error, so nothing on the trading path reports it
// until the pod dies.
//
// # Why eviction is safe, and what it actually costs
//
// envelope.proto: producer_sequence is "a per-(source, partition_key) monotonic
// counter, 1-based, used for consumer-side gap detection independent of the
// broker". Evicting a key restarts it at 1 — which is exactly what a PROCESS
// RESTART already does to every key at once, and the contract has always
// tolerated that. Nothing in this estate reads the field today: the only
// GetProducerSequence outside the generated SDK is one accounting test
// asserting two publishes on one key are 1 then 2, which a live key still
// satisfies.
//
// So the cost is bounded and stated: a future gap detector seeing a restart-to-1
// on an idle key must treat it as a producer restart rather than a gap. That is
// the same rule it needs for pod restarts, which are not optional.
//
// # Why lazy, capacity-triggered eviction and not a goroutine
//
// A sweeper goroutine would need starting and joining in EVERY composition root
// that builds a Producer — twenty-odd services — and a Close() on a type that
// has never had one. That is a large, error-prone change to fix a leak, and a
// forgotten join is its own defect. Sweeping on insert costs nothing on the
// common path (the key already exists), runs only when the map is at capacity,
// and cannot be forgotten because there is nothing to wire.
//
// The bound is HARD: len(sequence) never exceeds seqMax, because gc runs before
// any insert that would cross it and evicts by soonest-touched when everything
// is still live. The TTL decides WHICH keys go first; the cap is what makes the
// memory answer finite regardless of burst rate. Same discipline, same shape and
// the same two knobs as DedupWindow.gc one file over.
const (
	// defaultSequenceTTL is how long an untouched key is kept. An order's FACTs
	// land within seconds of each other; an hour is generous for a redelivery or
	// a late cancel and still sheds a day's order ids many times over.
	defaultSequenceTTL = time.Hour
	// defaultSequenceMax is the hard ceiling on live keys. At the ~100-150 bytes
	// an entry costs, 100k keys is ~10-15MB — a bound an operator can reason
	// about, and far above any burst a single process sees inside the TTL.
	defaultSequenceMax = 100_000
)

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
	ttl, max := cfg.SequenceTTL, cfg.SequenceMax
	if ttl <= 0 {
		ttl = defaultSequenceTTL
	}
	if max <= 0 {
		max = defaultSequenceMax
	}
	return &Producer{
		client:       client,
		cfg:          cfg,
		metrics:      cfg.Metrics,
		verifyIssuer: cfg.VerifyCommandIssuer,
		sequence:     make(map[seqKey]seqEntry),
		seqTTL:       ttl,
		seqMax:       max,
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
	// NATS JetStream keys its broker-side dedup window on Nats-Msg-Id
	// (EVT-08); Kafka treats this as an ordinary user header (harmless).
	// One header serves both transports because NATS reserves the name
	// and Kafka is name-agnostic. A constant, not a literal, because the
	// redrive path has to RE-STAMP it (see redriveMsgID) and a second
	// spelling of a reserved name fails silently — the publish succeeds and
	// simply stops being deduplicated.
	headers := map[string]string{headerNatsMsgID: env.IdempotencyKey}
	if e.ExpectedLastSubjectSeq != nil {
		headers[headerExpectedLastSubjectSeq] = strconv.FormatUint(*e.ExpectedLastSubjectSeq, 10)
	}
	return p.client.Publish(ctx, Message{
		Subject: e.Subject,
		Key:     []byte(env.PartitionKey),
		Body:    body,
		Headers: headers,
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
	now := p.seqClock()
	k := seqKey{eventType, partitionKey}
	e, live := p.sequence[k]
	// SWEPT ONLY WHEN A NEW KEY WOULD CROSS THE CEILING. A publish for a key the
	// map already holds — the common case, every FACT after an order's first —
	// touches nothing but its own entry.
	if !live && len(p.sequence) >= p.seqMax {
		p.gcSequence(now)
	}
	e.seq++
	e.touched = now
	p.sequence[k] = e
	// NEVER 0 ON AN EVICTED KEY. Validate refuses producer_sequence == 0 while
	// partition_key is non-empty, so an eviction that returned 0 would turn a
	// memory bound into a publish failure. The zero seqEntry increments to 1,
	// which is the same value a fresh process would have stamped.
	return e.seq
}

// gcSequence sheds expired keys, then the soonest-touched until the map is under
// its ceiling. Caller holds p.seqMu.
//
// It is DedupWindow.gc's discipline over a different value, and the second half
// is what makes the bound hard: a burst that creates seqMax live keys inside the
// TTL still cannot grow the map, it just evicts the least recently used. Without
// it the TTL alone would be a hope rather than a limit.
func (p *Producer) gcSequence(now time.Time) {
	for k, e := range p.sequence {
		if now.Sub(e.touched) >= p.seqTTL {
			delete(p.sequence, k)
		}
	}
	for len(p.sequence) >= p.seqMax {
		var oldestK seqKey
		var oldestT time.Time
		first := true
		for k, e := range p.sequence {
			if first || e.touched.Before(oldestT) {
				oldestK, oldestT, first = k, e.touched, false
			}
		}
		delete(p.sequence, oldestK)
	}
}

// seqClock is the sequence window's time source. Caller holds p.seqMu (or is the
// constructor).
func (p *Producer) seqClock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

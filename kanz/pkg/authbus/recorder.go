// Package authbus is the wiring adapter that publishes AUTH-01d authorization
// decisions onto the bus (PARITY-04f), keeping pkg/auth transport-decoupled (the
// AUTH-01c stance: auth defines the DecisionRecorder seam + BuildDecisionLog
// mapping, the bus binding lives here). BusRecorder publishes every allow/deny
// DecisionLog as an OBSERVATION event on the durable platform.authz.decision
// subject so AUDIT-01's append-only projection is the system of record —
// replacing SlogRecorder (which only reaches stdout) at any composition root
// that has a bus.Producer.
package authbus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
)

const (
	schemaRefDecisionLog     = "observation.v1.DecisionLog:1"
	schemaVersionDecisionLog = 1
	// defaultQueueSize buffers decision records so Record never blocks the
	// request hot path under normal load; sized for bursty authz traffic.
	defaultQueueSize = 1024
	// publishTimeout bounds each background publish so a wedged bus can't pin a
	// worker forever (the request is already returned; this is the async tail).
	publishTimeout = 5 * time.Second
)

// BusRecorder is a bus-backed auth.DecisionRecorder. Record is non-blocking (the
// AUTH-01d contract: it runs on the request hot path) — it enqueues onto a
// buffered channel drained by a background worker that maps each DecisionLog onto
// an OBSERVATION event and publishes it. This matches AuditedAuthorizer's own
// best-effort stance: an audit-sink outage or a burst overflowing the buffer
// drops the record (reported via the onOverflow hook + a counter), never stalls
// or fails the service. Under healthy operation nothing is dropped and AUDIT-01's
// projection is the durable system of record.
type BusRecorder struct {
	producer *bus.Producer
	ctx      context.Context
	now      func() time.Time
	queue    chan *observationpb.DecisionLog
	onErr    func(err error)
	onDrop   func(*observationpb.DecisionLog)
	tenant   string

	wg       sync.WaitGroup
	stopOnce sync.Once
	dropped  atomic.Int64
}

// Option customizes a BusRecorder.
type Option func(*BusRecorder)

// WithQueueSize sets the buffer depth (default defaultQueueSize). Larger buffers
// absorb bigger bursts before dropping.
func WithQueueSize(n int) Option {
	return func(r *BusRecorder) {
		if n > 0 {
			r.queue = make(chan *observationpb.DecisionLog, n)
		}
	}
}

// WithClock injects the event_time clock (tests). Nil ⇒ time.Now.
func WithClock(now func() time.Time) Option {
	return func(r *BusRecorder) {
		if now != nil {
			r.now = now
		}
	}
}

// WithErrorHandler sets the hook invoked on a background publish error.
func WithErrorHandler(fn func(err error)) Option {
	return func(r *BusRecorder) { r.onErr = fn }
}

// WithOverflowHandler sets the hook invoked when the buffer is full and a record
// is dropped (so a deployment can alert on lost audit records).
func WithOverflowHandler(fn func(*observationpb.DecisionLog)) Option {
	return func(r *BusRecorder) { r.onDrop = fn }
}

// WithFallbackTenant sets the envelope tenant_id used for decisions that carry
// NO principal tenant of their own — an unauthenticated caller, or a token whose
// tenant claim is absent.
//
// It is a fallback and not the tenant: publish stamps each decision with the
// DECIDING PRINCIPAL'S tenant when there is one (see tenantFor). That ordering
// matters at a multi-tenant composition root — a per-request fallback would
// attribute every customer's access decisions to whatever the deployment was
// configured with, a value that is valid and not theirs, which nothing
// downstream can detect.
//
// WHY THIS EXISTS HERE RATHER THAN AS ProducerConfig.Tenant. bus.Validate
// requires tenant_id on the live path, and an authorization decision is raised
// by an inbound HTTP request with no delivery to inherit a ctx tenant from. The
// obvious fix — a producer-level fallback — is WRONG at any composition root
// that shares one producer with a capital path: api-gateway publishes order
// COMMANDs through the same producer, and a fallback there would silently stamp
// a submit whose tenant claim was missing with the gateway's own tenant instead
// of refusing it. Stamping per event keeps the fallback scoped to decisions.
func WithFallbackTenant(t string) Option {
	return func(r *BusRecorder) { r.tenant = t }
}

// NewBusRecorder builds a bus-backed recorder over producer and starts its
// worker. Call Close at shutdown to drain the buffer. Returns an error when
// producer is nil (use SlogRecorder when no bus is available).
func NewBusRecorder(producer *bus.Producer, opts ...Option) (*BusRecorder, error) {
	if producer == nil {
		return nil, errors.New("authbus: producer is nil")
	}
	r := &BusRecorder{
		producer: producer,
		ctx:      context.Background(),
		now:      time.Now,
		queue:    make(chan *observationpb.DecisionLog, defaultQueueSize),
	}
	for _, o := range opts {
		o(r)
	}
	r.wg.Add(1)
	go r.run()
	return r, nil
}

var _ auth.DecisionRecorder = (*BusRecorder)(nil)

// Record enqueues the DecisionLog for asynchronous publish. Non-blocking: on a
// full buffer it drops (reported) rather than stalling the authorizer. Always
// returns nil so AuditedAuthorizer never logs a per-request audit failure for
// the common enqueue path.
func (r *BusRecorder) Record(_ context.Context, entry *observationpb.DecisionLog) error {
	if entry == nil {
		return nil
	}
	select {
	case r.queue <- entry:
	default:
		r.dropped.Add(1)
		if r.onDrop != nil {
			r.onDrop(entry)
		}
	}
	return nil
}

// Dropped is the count of records shed on buffer overflow (a gauge for alerts).
func (r *BusRecorder) Dropped() int64 { return r.dropped.Load() }

// Close stops accepting records, drains the buffer, and waits for the worker.
// Idempotent.
func (r *BusRecorder) Close() {
	r.stopOnce.Do(func() { close(r.queue) })
	r.wg.Wait()
}

func (r *BusRecorder) run() {
	defer r.wg.Done()
	for entry := range r.queue {
		r.publish(entry)
	}
}

// publish maps a DecisionLog onto an OBSERVATION event on platform.authz.decision
// and publishes it. Partitioned by the deciding principal so a subject's audit
// trail stays ordered.
func (r *BusRecorder) publish(entry *observationpb.DecisionLog) {
	ctx, cancel := context.WithTimeout(r.ctx, publishTimeout)
	defer cancel()
	err := r.producer.Publish(ctx, bus.Event{
		Subject:          auth.AuthzDecisionEventType,
		EventType:        auth.AuthzDecisionEventType,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_OBSERVATION,
		SchemaVersion:    schemaVersionDecisionLog,
		Domain:           auth.AuthzDecisionDomain,
		EventTime:        r.now(),
		PartitionKey:     partitionKey(entry),
		PayloadSchemaRef: schemaRefDecisionLog,
		Payload:          entry,
		TenantID:         r.tenantFor(entry),
	})
	if err != nil && r.onErr != nil {
		r.onErr(err)
	}
}

// tenantFor is the envelope tenant_id for one decision: the DECIDING PRINCIPAL'S
// tenant, falling back to WithFallbackTenant.
//
// A decision about acme's user reading acme's data belongs to acme, and stamping
// it explicitly is what keeps a shared producer safe — the alternative,
// ProducerConfig.Tenant, applies to every event that producer sends, including
// capital commands on the same connection.
//
// Empty is returned when the decision has no principal tenant and no fallback was
// configured. That is deliberate and LOUD: bus.Validate rejects an empty
// tenant_id on the live path, so the publish fails, the error handler counts it,
// and the deployment learns it never configured one. Defaulting silently here
// would attribute those decisions to a tenant nobody chose.
func (r *BusRecorder) tenantFor(entry *observationpb.DecisionLog) string {
	if t := entry.GetAttributes()["principal.tenant"]; t != "" {
		return t
	}
	return r.tenant
}

// partitionKey keeps a principal's decisions on one partition (ordered). Falls
// back to the tenant, then empty (round-robin) when neither is present.
func partitionKey(entry *observationpb.DecisionLog) string {
	attrs := entry.GetAttributes()
	if s := attrs["principal.subject"]; s != "" {
		return s
	}
	return attrs["principal.tenant"]
}

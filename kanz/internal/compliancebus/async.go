package compliancebus

import (
	"context"
	"errors"
	"log/slog"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/decisionq"
)

// THE PRE-TRADE GATE'S DECISIONS REACH THE AUDIT CHAIN WITHOUT WAITING FOR THE
// BROKER (#713).
//
// # Why the synchronous recorder above is the wrong shape for this caller
//
// BusRecorder publishes inline, which suits the post-trade monitor: it runs on a
// fold of position FACTs, not on anybody's hot path. The OMS's pre-trade gate is
// the opposite. Order admission is deliberately outbox-backed — every FACT is
// written in the same transaction as the order and drained by a relay — so
// nothing on that path waits for a broker today. Adding a synchronous publish in
// front of every order would put broker latency into admission: a change to the
// TRADING path, made for a REPORTING reason.
//
// So this one queues. Record returns immediately, a background worker publishes,
// and an audit-sink outage costs records rather than orders.
//
// # WHAT IS LOST, AND WHY IT IS THE RIGHT TRADE
//
// A queued record can be dropped: the buffer is bounded, because an unbounded one
// turns a wedged broker into unbounded memory growth in the OMS, which is a worse
// outage than the missing records. Every drop is counted and reported — see
// WithOverflowHandler — so "the trail is complete" and "the trail is missing the
// busiest ten minutes of the day" are different numbers rather than the same
// silence.
//
// # THE TENANT COMES OFF THE RECORD, NOT OFF THE CONTEXT
//
// This is the defect that makes an async recorder different in kind from a sync
// one. pkg/bus stashes the inbound delivery's tenant on ctx, and every
// synchronous publisher in this estate reads it from there. By the time this
// worker runs, that context is gone — and the OMS's producer carries no tenant
// fallback, so the broker refuses the event outright. That refusal crash-looped
// the OMS once already. comp.DecisionRecord.TenantID exists for this.

// AsyncRecorder publishes compliance decisions off the caller's goroutine.
type AsyncRecorder struct {
	b       Bus
	logger  *slog.Logger
	now     func() time.Time
	queue   *decisionq.Queue[comp.DecisionRecord]
	size    int
	onDrop  func(comp.DecisionRecord)
	onError func(error)
}

// AsyncOption customizes an AsyncRecorder.
type AsyncOption func(*AsyncRecorder)

// WithQueueSize overrides decisionq.DefaultSize.
func WithQueueSize(n int) AsyncOption {
	return func(r *AsyncRecorder) { r.size = n }
}

// WithOverflowHandler reports a decision shed because the buffer was full.
//
// WIRE IT. Without it a drop is invisible, and the state it hides — a broker that
// has stopped accepting while the gate keeps admitting orders — is exactly the one
// an operator needs to see, because the ORDERS are still being placed.
func WithOverflowHandler(fn func(comp.DecisionRecord)) AsyncOption {
	return func(r *AsyncRecorder) { r.onDrop = fn }
}

// WithErrorHandler reports a publish that failed after dequeue. Separate from
// overflow because the causes and the fixes differ: overflow is this service
// producing faster than the broker accepts, an error is the broker refusing.
func WithErrorHandler(fn func(error)) AsyncOption {
	return func(r *AsyncRecorder) { r.onError = fn }
}

// WithClock injects the clock (tests).
func WithClock(now func() time.Time) AsyncOption {
	return func(r *AsyncRecorder) { r.now = now }
}

// NewAsyncRecorder starts the recorder and its worker.
//
// IT RETURNS AN ERROR RATHER THAN A WORKING-LOOKING RECORDER on a nil bus: the
// alternative degrades to a queue that publishes nowhere, which is
// indistinguishable at the gate from one that is keeping up.
func NewAsyncRecorder(b Bus, logger *slog.Logger, opts ...AsyncOption) (*AsyncRecorder, error) {
	if b == nil {
		return nil, errors.New("compliancebus: bus is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &AsyncRecorder{b: b, logger: logger, now: time.Now, size: decisionq.DefaultSize}
	for _, o := range opts {
		if o != nil {
			o(r)
		}
	}
	r.queue = decisionq.New(r.publish,
		decisionq.WithSize[comp.DecisionRecord](r.size),
		decisionq.WithOverflowHandler(func(rec comp.DecisionRecord) {
			if r.onDrop != nil {
				r.onDrop(rec)
			}
			r.logger.Warn("a compliance decision was DROPPED before it reached the audit trail — "+
				"the recorder queue is full, so the trail is missing decisions this process did "+
				"make while the orders they gated went ahead",
				"phase", rec.Phase, "order_id", rec.OrderID,
				"portfolio_id", rec.Result.GetPortfolioId(), "subject", SubjectDecision)
		}))
	return r, nil
}

var _ comp.DecisionRecorder = (*AsyncRecorder)(nil)

// Record enqueues the decision and returns. It NEVER returns an error: there is
// nothing the gate could do with one, and the enforcement points call this
// best-effort by design. What must not happen is a nil result reaching the
// worker, so an unusable record is refused here where the caller's context still
// exists to name it.
func (r *AsyncRecorder) Record(_ context.Context, rec comp.DecisionRecord) error {
	if rec.Result == nil {
		r.logger.Error("a compliance decision with no result was not recorded — the record cannot "+
			"be stamped with an evaluation time and the broker would refuse it",
			"phase", rec.Phase, "order_id", rec.OrderID)
		return nil
	}
	r.queue.Enqueue(rec)
	return nil
}

// Dropped is how many decisions the buffer has shed.
func (r *AsyncRecorder) Dropped() int64 { return r.queue.Dropped() }

// Close drains the queue and stops the worker. Idempotent.
//
// CALL IT ON SHUTDOWN. The decisions still buffered gated orders that were
// admitted; discarding them would leave the trail short precisely across deploys.
func (r *AsyncRecorder) Close() { r.queue.Close() }

// publish maps one record and sends it.
//
// THE SUBJECT IS A LITERAL CONSTANT IN THIS PACKAGE, and it must stay one:
// test/arch/nats_service_permissions_test.go derives each service's publish grant
// from the subjects its code names, resolving a literal or a package-level
// constant and nothing else. A subject threaded through a field would resolve to
// nothing, and the guard would stop checking that whoever publishes this is
// allowed to.
func (r *AsyncRecorder) publish(rec comp.DecisionRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	err := r.b.Publish(ctx, bus.Event{
		Subject:       SubjectDecision,
		EventType:     DecisionEventType,
		EventClass:    envelopepb.EventClass_EVENT_CLASS_OBSERVATION,
		SchemaVersion: 1,
		Domain:        DecisionDomain,
		// THE EVALUATION'S OWN TIME, never the publish time — the audit store is
		// queried by when a decision was MADE, so stamping now would make a
		// delayed or replayed decision look current. That matters more here than
		// on the synchronous path: this one is delayed BY DESIGN.
		EventTime:        rec.Result.GetEvaluatedAt().AsTime(),
		PartitionKey:     rec.Result.GetPortfolioId(),
		PayloadSchemaRef: "observation.v1.DecisionLog:1",
		Payload:          BuildDecisionLog(rec),
		// OFF THE RECORD, NOT OFF THE CONTEXT — see the file doc. An empty tenant
		// is refused by bus.Validate, which is the loud failure this field exists
		// to avoid and NOT one to paper over with a default: a decision about
		// acme's portfolio filed under another tenant is valid, wrong and
		// undetectable downstream.
		TenantID: rec.TenantID,
	})
	if err == nil {
		return
	}
	if r.onError != nil {
		r.onError(err)
	}
	r.logger.Error("a compliance decision could not be published — it exists only in this log line",
		"err", err, "phase", rec.Phase, "order_id", rec.OrderID,
		"portfolio_id", rec.Result.GetPortfolioId(), "tenant_id", rec.TenantID,
		"subject", SubjectDecision)
}

// publishTimeout bounds each background publish so a wedged broker cannot pin the
// worker for ever. The decision has already been acted on; this is the async tail.
const publishTimeout = 5 * time.Second

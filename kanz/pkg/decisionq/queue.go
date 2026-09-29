// Package decisionq is the bounded, non-blocking queue that sits between a
// decision recorder and the bus.
//
// # Why it exists as its own package (#713)
//
// Two enforcement points record decisions on a HOT PATH — the gateway's
// authorization check, on every request, and the OMS's pre-trade compliance gate,
// on every order. Both must record durably and neither may wait for a broker: an
// audit-sink outage must not become a service outage, and on the OMS an added
// synchronous publish would put broker latency into order admission, which is a
// change to the trading path made for a reporting reason.
//
// pkg/authbus built exactly this machinery for the first of them. The second one
// arriving is AGENTS.md's promotion rule — "shared code is promoted when a SECOND
// consumer appears" — and the alternative is the failure that rule exists to
// prevent: a copied queue where a fix to one overflow accounting, one shutdown
// drain or one publish timeout stops spreading.
//
// # WHAT THIS PACKAGE DELIBERATELY DOES NOT DO
//
// IT DOES NOT PUBLISH. It takes a func and calls it on a worker. That is not
// indirection for its own sake: test/arch/nats_service_permissions_test.go derives
// each service's PUBLISH set from the subject literals in its code, and it
// resolves a literal or a named constant — not a struct field. A shared publisher
// parameterised by subject would resolve to nothing, and the guard would go quiet
// for the gateway's authorization decisions as well as for compliance. So the
// subject stays a literal in each caller's own publish function, and only the
// queueing is shared.
package decisionq

import (
	"sync"
	"sync/atomic"
)

// DefaultSize buffers records so Enqueue never blocks the hot path under normal
// load; sized for bursty traffic on the busiest of the two callers.
const DefaultSize = 1024

// Queue is a bounded FIFO drained by one background worker.
//
// THE BOUND IS THE POINT. An unbounded queue turns a wedged broker into
// unbounded memory growth on the service that was trying to stay up, which is a
// worse outage than the dropped records. Overflow is counted and reported, never
// silent: a drop nobody counts is an audit trail with holes nobody can see.
type Queue[T any] struct {
	ch       chan T
	publish  func(T)
	onDrop   func(T)
	wg       sync.WaitGroup
	stopOnce sync.Once
	dropped  atomic.Int64
}

// Option customizes a Queue.
type Option[T any] func(*Queue[T])

// WithSize overrides DefaultSize. A non-positive size is IGNORED rather than
// treated as unbuffered: a zero-capacity channel would make every Enqueue drop
// unless a worker happened to be waiting, turning a tuning mistake into a silent
// loss of the whole audit trail.
func WithSize[T any](n int) Option[T] {
	return func(q *Queue[T]) {
		if n > 0 {
			q.ch = make(chan T, n)
		}
	}
}

// WithOverflowHandler reports each record shed because the buffer was full. The
// caller counts it; this package only guarantees the hook fires.
func WithOverflowHandler[T any](fn func(T)) Option[T] {
	return func(q *Queue[T]) { q.onDrop = fn }
}

// New starts a queue whose worker calls publish for each record, in order.
//
// ONE WORKER, NOT A POOL, and that is a correctness choice rather than a
// simplification: both callers partition their events by subject so one
// principal's or one portfolio's trail stays ordered, and a pool would reorder
// records that a reader reconstructs a sequence from.
func New[T any](publish func(T), opts ...Option[T]) *Queue[T] {
	q := &Queue[T]{ch: make(chan T, DefaultSize), publish: publish}
	for _, o := range opts {
		if o != nil {
			o(q)
		}
	}
	q.wg.Add(1)
	go q.run()
	return q
}

// Enqueue accepts a record for publication and NEVER BLOCKS. On a full buffer it
// drops, counts and reports.
func (q *Queue[T]) Enqueue(v T) {
	select {
	case q.ch <- v:
	default:
		q.dropped.Add(1)
		if q.onDrop != nil {
			q.onDrop(v)
		}
	}
}

// Dropped is how many records the buffer has shed.
func (q *Queue[T]) Dropped() int64 { return q.dropped.Load() }

// Close stops accepting records, DRAINS what is already queued, and waits for the
// worker.
//
// DRAINING RATHER THAN DISCARDING is what makes a graceful shutdown honest: the
// decisions still in the buffer were made and acted upon, and dropping them on
// the way out would leave the trail short exactly on the deploys and restarts
// somebody will later want to reconstruct. Idempotent.
func (q *Queue[T]) Close() {
	q.stopOnce.Do(func() { close(q.ch) })
	q.wg.Wait()
}

func (q *Queue[T]) run() {
	defer q.wg.Done()
	for v := range q.ch {
		q.publish(v)
	}
}

package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/pkg/bus"
)

// Publisher is the publish surface the relay needs — satisfied by *bus.Producer.
type Publisher interface {
	Publish(ctx context.Context, e bus.Event) error
}

// defaultBatch bounds how many records one key contributes to a pass, and how
// many keys a pass looks at. It exists so a backlog cannot turn one drain into
// an unbounded loop holding a connection; the next pass picks up where this one
// stopped, and the age gauge shows the backlog draining.
const defaultBatch = 256

// Relay drains the outbox and publishes what it finds.
//
// # DECISION: IT IS AN IN-PROCESS GOROUTINE, NOT A SIDECAR AND NOT AN
// ESTATE-WIDE BINARY
//
// Three shapes were available.
//
//   - A SIDECAR multiplies by 26. Every service that adopts an outbox gains a
//     container, a probe, a resource request and a NetworkPolicy edge to
//     Postgres — and the sidecar needs the SAME tenant-scoped DSN the service
//     already holds, from the same Vault path, so it duplicates the credential
//     without isolating anything. It buys process separation from a component
//     whose failure mode (stops draining) is one the service must survive
//     anyway.
//
//   - ONE ESTATE-WIDE RELAY BINARY cannot read the table. This is not a
//     preference; it is a finding. internal/pg.NewTenantPool is the ONLY pool
//     constructor on this platform and it REFUSES an empty tenant, pinning
//     app.tenant_id session-wide on every connection. The outbox table is
//     FORCE ROW LEVEL SECURITY with a policy on app_current_tenant(), and
//     app_current_tenant() RAISES on an unscoped session (MT-01e). The app role
//     is NOSUPERUSER (CI creates it that way and test/arch/
//     postgres_dev_convention_test.go pins it), so it cannot bypass RLS either.
//     A single relay would therefore need one pool per tenant and a registry of
//     every tenant to build them from — a registry that does not exist — or a
//     superuser role, which is precisely the thing the isolation posture forbids.
//     A relay that could read every tenant's outbox is one bug away from
//     publishing tenant A's FACT under tenant B's envelope, which is the most
//     serious defect class in this codebase.
//
//   - AN IN-PROCESS GOROUTINE inherits the pool the service already opened, so
//     it is scoped to exactly the tenant whose orders it is announcing, by
//     construction and with nothing new to configure. It shares the service's
//     lifecycle, so it stops before the pool closes. Its cost is that it stops
//     when the pod stops — which is fine, because the records are in Postgres
//     and the OTHER replica (oms-deploy.yaml runs replicas: 2) drains them.
//
// WHAT CHANGES WHEN A SECOND SERVICE ADOPTS THIS: nothing about the shape. This
// package moves to kanz/internal/outbox at that point (CLAUDE.md's promotion
// rule), each service runs its own Relay over its own pool and its own table,
// and the per-key lock namespace — "<tenant>/oms/outbox" in
// Postgres.LockKey — becomes "<tenant>/<service>/outbox". The one thing that
// must NOT happen is a single relay growing cross-tenant or cross-service
// database access to save a goroutine.
type Relay struct {
	q      Queue
	pub    Publisher
	logger *slog.Logger

	interval time.Duration
	batch    int
	now      func() time.Time

	published prometheus.Counter
	failures  prometheus.Counter
}

// RelayOption customizes the relay.
type RelayOption func(*Relay)

// WithInterval sets the background drain tick. It bounds how late a FACT can be
// when the handler's own Flush did not get it out — a publish that failed, an
// enqueue site that forgets to flush, or a record left behind by a pod that died
// mid-drain and is now this pod's to finish.
func WithInterval(d time.Duration) RelayOption {
	return func(r *Relay) {
		if d > 0 {
			r.interval = d
		}
	}
}

// WithBatch bounds records per key and keys per pass.
func WithBatch(n int) RelayOption {
	return func(r *Relay) {
		if n > 0 {
			r.batch = n
		}
	}
}

// WithCounters gives the relay its published/failed counters.
//
// A publish failure here is NOT an error anybody sees: the handler that enqueued
// the record has long since returned nil, the command was acked, and the row is
// durable. The counter is the only thing that distinguishes a relay working
// through a broker blip from a relay that has not published anything since
// Tuesday.
func WithCounters(published, failures prometheus.Counter) RelayOption {
	return func(r *Relay) {
		r.published = published
		r.failures = failures
	}
}

// NewRelay wires a relay over a queue and a publisher.
func NewRelay(q Queue, pub Publisher, logger *slog.Logger, opts ...RelayOption) (*Relay, error) {
	if q == nil || pub == nil {
		return nil, errors.New("outbox: relay needs a queue and a publisher")
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &Relay{
		q: q, pub: pub, logger: logger,
		interval: time.Second, batch: defaultBatch, now: time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// Run drains until ctx is cancelled.
//
// IT DOES ONE PASS BEFORE THE FIRST TICK. A pod that starts holding records its
// predecessor enqueued and died before publishing must not wait an interval to
// find out — and on a cold start after a crash that backlog is exactly the set
// of FACTs the estate is missing.
//
// It returns nil on cancellation and an error only on a fault that makes
// draining impossible at all; the caller treats that as fatal, because an OMS
// that is admitting orders while nothing publishes their FACTs is trading
// invisibly.
func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		if _, err := r.DrainOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			// A pass that failed is not fatal — the records are still in the
			// table and the next pass retries them. It IS loud: this is the
			// only place a stalled outbox is reported before the age gauge
			// crosses an alert threshold.
			r.logger.Error("oms: an outbox drain pass failed — order FACTs are committed and NOT yet "+
				"published; they stay in the outbox and the next pass retries them", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// DrainOnce runs one background pass and returns how many records it published.
//
// A key it cannot fully drain — the publish failed, or another replica holds it
// — does NOT fail the pass. That is the difference from Flush below: this is the
// safety net, and one stuck order must not shadow every order behind it. (The
// same reasoning, and the same shape, as the periodic sweep's decision to
// continue past a poison order rather than abort on it.)
//
// Exported because the ONLY honest way to test a relay is to run the relay: a
// test that reimplemented the drain would be asserting against itself. The
// service-level tests call this synchronously where production's goroutine
// calls it on a tick — the same function, driven differently.
func (r *Relay) DrainOnce(ctx context.Context) (int, error) {
	keys, err := r.q.PendingKeys(ctx, r.batch)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, key := range keys {
		if ctx.Err() != nil {
			return sent, ctx.Err()
		}
		n, _, err := r.drainKey(ctx, key, false)
		sent += n
		if err != nil {
			return sent, err
		}
	}
	return sent, nil
}

// Flush publishes everything committed for ONE order and returns an error
// unless the queue for that order is empty afterwards.
//
// # WHY A HANDLER CALLS THIS SYNCHRONOUSLY, AND WHY THE ERROR MATTERS
//
// #292 is converting the OMS one transition at a time: admission's ACCEPTED FACT
// goes through the outbox, and the FACTs that follow it on the same order —
// ROUTED, the fills, the outcome — are still published directly by the handler.
// A relay draining on a ticker would let a directly-published ORDER_ROUTED
// overtake a queued ORDER_ACCEPTED, and tv-sync's transition() DROPS a routed
// FACT for an order it never admitted: the projection would go permanently blind
// to a live order. Late is survivable; out of order is not.
//
// So the handler that committed the FACT publishes it before it publishes
// anything else for that order — in the same goroutine, in the same sequence the
// old synchronous EmitAccepted produced. The failure behaviour is identical to
// that call's too: the handler returns the error, does not work the order, and
// the command nacks. What is DIFFERENT, and is the whole point, is that the FACT
// is already durable, so the retry publishes the original instead of needing a
// compensator to notice one went missing.
//
// IT WAITS FOR THE KEY RATHER THAN TRYING. Contention here is the background
// pass having reached the same order microseconds earlier; nacking a live
// trading command over that would be a self-inflicted DLQ entry.
//
// WHAT IT COSTS, SAID PLAINLY: a lock, a read, one publish per record, one
// UPDATE per record and an unlock — so four database round trips plus the
// publish on the admission path, and four on a cancel or an amend whose order
// has nothing queued at all. It is deliberately NOT short-circuited by an
// unlocked "is anything pending?" read first: that check would be answered from
// a snapshot taken before the lock, and a record committed in the gap would be
// exactly the one a concurrent admission just wrote. Buying three round trips
// with a race on the capital path is the trade this repository keeps finding in
// its own history.
//
// THIS CALL DISAPPEARS WHEN THE LAST DIRECT PUBLISH DOES. Once every FACT on an
// order goes through the outbox, the relay is the only publisher and there is
// nothing left for a direct publish to overtake — at which point Flush becomes a
// latency optimization and its error a warning. It is a synchronous blocking
// call today because the conversion is partial, and that is stated here so the
// cost is not mistaken for the design.
func (r *Relay) Flush(ctx context.Context, key string) error {
	_, stalled, err := r.drainKey(ctx, key, true)
	if err != nil {
		return err
	}
	return stalled
}

// drainKey publishes one partition key's backlog, IN ORDER, STOPPING AT THE
// FIRST FAILURE.
//
// THE STOP IS THE POINT, NOT AN OPTIMIZATION. Skipping a record that will not
// publish and carrying on to the next one for the same key delivers a consumer
// its history out of sequence — an ORDER_ROUTED for an order tv-sync never
// admitted is DROPPED by transition(), so the projection stays blind to a live
// order while the OMS believes it announced everything. A late FACT is
// recoverable; a reordered fold is not. So the key blocks, the attempt count
// climbs, and the oldest-pending age gauge is what says so.
//
// Other keys are unaffected: the head-of-line is per key, which is the whole
// reason the lock is per key.
// It returns three things and the middle one is load-bearing: sent, a STALL
// (this key still holds unpublished records and the caller must not publish past
// it), and an infrastructure error. The background pass ignores the stall — one
// stuck order must not shadow the rest of the book — and Flush returns it,
// because its caller is about to publish the next FACT for this same order.
func (r *Relay) drainKey(ctx context.Context, key string, wait bool) (int, error, error) {
	release, ok, err := r.q.LockKey(ctx, key, wait)
	if err != nil {
		return 0, nil, err
	}
	if !ok {
		// Another replica owns this key's drain. Not an error, and not something
		// the background pass waits for — it is publishing the same records this
		// pass would have. Reported as a STALL so a waiting caller (which cannot
		// reach here: wait=true never returns ok=false) would not proceed blind.
		return 0, errors.New("outbox: another replica is draining " + key), nil
	}
	defer release()

	// READ UNDER THE LOCK. Reading before it would let the loser of the race
	// hold a snapshot the winner is already publishing from, and then publish
	// the same records again on the next pass for no reason.
	pending, err := r.q.Pending(ctx, key, r.batch)
	if err != nil {
		return 0, nil, err
	}
	sent := 0
	for _, p := range pending {
		event, err := p.Record.Event()
		if err == nil {
			err = r.pub.Publish(ctx, event)
		}
		if err != nil {
			if merr := r.q.MarkFailed(ctx, p.ID, err); merr != nil {
				return sent, nil, fmt.Errorf("outbox: %s could not record a failed attempt: %w", key, merr)
			}
			inc(r.failures)
			// ERROR, and it names the consequence rather than the call. Every
			// FACT behind this one on this order is now waiting on it.
			r.logger.Error("oms: an outbox record could not be published — every FACT behind it for this "+
				"order is held back until it goes out, because publishing past it would hand consumers "+
				"this order's history out of sequence",
				"partition_key", key, "event_type", p.Record.EventType,
				"outbox_id", p.ID, "attempts", p.Attempts+1, "err", err)
			return sent, fmt.Errorf("outbox: %s is stalled on %s (outbox id %d): %w",
				key, p.Record.EventType, p.ID, err), nil
		}
		if err := r.q.MarkPublished(ctx, p.ID); err != nil {
			// The broker HAS the event. Failing to record that is survivable —
			// the next pass republishes it, which is the at-least-once direction
			// every consumer of these FACTs already tolerates — but it must not
			// look like nothing happened, because a mark that keeps failing is a
			// relay republishing the same FACT forever.
			r.logger.Error("oms: an outbox record was PUBLISHED and could not be marked — it will be "+
				"published again on the next pass",
				"partition_key", key, "event_type", p.Record.EventType, "outbox_id", p.ID, "err", err)
			return sent, nil, err
		}
		inc(r.published)
		sent++
	}
	// A key with MORE records than the batch is not drained: the caller must not
	// treat a full batch as an empty queue. Report it as a stall so Flush waits
	// for the next call rather than letting a direct publish overtake the
	// remainder. Unreachable on the admission path (one record per order) and
	// cheap insurance if that ever stops being true.
	if len(pending) == r.batch {
		return sent, fmt.Errorf("outbox: %s still has records beyond the %d-record batch", key, r.batch), nil
	}
	return sent, nil, nil
}

// OldestPendingAge is the gauge source. See Queue.OldestPendingAge for why this
// number, and not a depth or an error rate, is the one that has to be alertable:
// an empty outbox and a relay that died both produce zero errors.
func (r *Relay) OldestPendingAge(ctx context.Context) float64 {
	age, ok, err := r.q.OldestPendingAge(ctx, r.now())
	if err != nil || !ok {
		return 0
	}
	return age.Seconds()
}

// inc is the nil-tolerant increment used above, so a relay constructed without
// metrics — every unit test, and any future caller that has not wired them yet —
// does not need a nil check at each call site. A missing counter must never be
// the reason a FACT is not published.
func inc(c prometheus.Counter) {
	if c != nil {
		c.Inc()
	}
}

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// Terminal admission outcomes for one order, as the OMS announces them.
//
// HARDCODED LIKE seed/ AND ingest/ DO, and for the same stated reason: the RISK-02
// boundary keeps a service's impl packages private to its own composer, so a
// harness under test/load names the well-known subject strings rather than
// importing services/oms/internal/order. They are the two outcomes of admission —
// order.order.pending_approval is deliberately NOT here, because a HELD order has
// no admission outcome yet and counting it as one would report a dual-control hold
// as an admitted order.
const (
	subjectAccepted = "order.order.accepted"
	subjectRejected = "order.order.rejected"
)

// factRec is what this harness saw for one order id.
type factRec struct {
	first time.Time
	kind  string
	// n is how many terminal FACTs carried this order id. TWO IS THE FINDING:
	// one intent that produced two announcements is what a redelivery overtaking
	// a still-running handler looks like from outside, and it is the failure mode
	// #865 says a latency-only harness would miss.
	n int
}

// ledger folds the order FACT stream by order id.
type ledger struct {
	mu   sync.Mutex
	seen map[string]*factRec
	// runTag is this run's order-id prefix, and `mine` counts the terminal FACTs
	// carrying it. Together with the submitter's 202 count they give an EXACT,
	// sub-second view of how many orders are submitted and not yet announced —
	// the outstanding depth, measured from the client with no broker
	// introspection and no metric resolution to caveat.
	runTag string
	mine   int64
}

// watchFacts arms the FACT reader and returns once the retained backlog has been
// replayed, so nothing this run submits can be missed.
//
// SubscribeReplay, NOT SubscribeBroadcast. A broadcast subscription delivers
// DeliverLastPerSubject — the LAST message per subject — and every order FACT of
// a kind shares one subject, so a broadcast would deliver exactly one accepted
// FACT for the whole run. It is also ephemeral, which is what this harness wants:
// a durable group would leave a consumer behind on the estate's own stream after
// every run.
//
// THE COST IS THE REPLAY. DeliverAll re-reads whatever the stream still retains
// before going live, which on a long-lived broker is every order FACT in the
// window. It is bounded work, it happens once, and the alternative — starting at
// "new" — cannot distinguish "the FACT arrived before we were listening" from
// "the platform never announced this order", which is the one conclusion this
// harness must never get wrong.
func watchFacts(ctx context.Context, cfg config, runTag string) (*ledger, func(), error) {
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.natsURL, Name: "load-orderflow"})
	if err != nil {
		return nil, nil, fmt.Errorf("dial nats %s: %w", cfg.natsURL, err)
	}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	l := &ledger{seen: map[string]*factRec{}, runTag: runTag}

	subCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	armed := make(chan error, 2)
	for _, subject := range []string{subjectAccepted, subjectRejected} {
		kind := subject
		ready := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := consumer.SubscribeReplay(subCtx, kind, l.handler(kind), func() { close(ready) })
			if err != nil && !errors.Is(err, context.Canceled) {
				select {
				case armed <- fmt.Errorf("subscribe %s: %w", kind, err):
				default:
				}
			}
		}()
		select {
		case <-ready:
		case err := <-armed:
			cancel()
			wg.Wait()
			_ = client.Close()
			return nil, nil, err
		case <-time.After(2 * time.Minute):
			cancel()
			wg.Wait()
			_ = client.Close()
			return nil, nil, fmt.Errorf("the replay of %q did not drain in 2m — this harness cannot tell a "+
				"missed announcement from an unadmitted order without it", kind)
		}
	}
	return l, func() { cancel(); wg.Wait(); _ = client.Close() }, nil
}

// handler folds one FACT. It reads the ORDER ID OFF THE ENVELOPE'S PARTITION KEY
// rather than decoding the payload: the emitter sets PartitionKey to the order id
// for every lifecycle FACT, and a harness that decoded order.v1 payloads would
// break the moment a FACT's payload type changed while its identity did not.
func (l *ledger) handler(kind string) bus.EventHandler {
	return func(_ context.Context, env *envelopepb.Envelope, _ []byte) error {
		id := env.GetPartitionKey()
		if id == "" {
			return nil
		}
		now := time.Now()
		l.mu.Lock()
		defer l.mu.Unlock()
		if r, ok := l.seen[id]; ok {
			r.n++
			return nil
		}
		l.seen[id] = &factRec{first: now, kind: kind, n: 1}
		if strings.HasPrefix(id, l.runTag) {
			l.mine++
		}
		return nil
	}
}

func (l *ledger) lookup(id string) (factRec, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.seen[id]
	if !ok {
		return factRec{}, false
	}
	return *r, true
}

// awaitStats is what one stage's submissions resolved to.
type awaitStats struct {
	admitted   int
	refused    int
	duplicated int
	missing    int
	p50        time.Duration
	p95        time.Duration
	p99        time.Duration
}

// await waits until every submission has a terminal FACT or the deadline passes,
// then reports the outcome.
//
// AN UNRESOLVED SUBMISSION IS COUNTED, NEVER DROPPED. It is the whole reason this
// harness reads the bus at all: the gateway answers 202 when the COMMAND is
// published, so an OMS that has fallen far enough behind to never admit the order
// is invisible to an HTTP-only load test — every request succeeds and the
// platform is not admitting anything.
func (l *ledger) await(ctx context.Context, subs []submission, deadline time.Time) awaitStats {
	for {
		pending := 0
		for _, s := range subs {
			if _, ok := l.lookup(s.id); !ok {
				pending++
			}
		}
		if pending == 0 || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
	}

	var st awaitStats
	lat := make([]time.Duration, 0, len(subs))
	for _, s := range subs {
		r, ok := l.lookup(s.id)
		if !ok {
			st.missing++
			continue
		}
		if r.n > 1 {
			st.duplicated++
		}
		if r.kind == subjectAccepted {
			st.admitted++
		} else {
			st.refused++
		}
		if d := r.first.Sub(s.at); d > 0 {
			lat = append(lat, d)
		}
	}
	st.p50, st.p95, st.p99 = percentiles(lat)
	return st
}

// announced is how many of THIS run's orders have a terminal FACT so far.
func (l *ledger) announced() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.mine
}

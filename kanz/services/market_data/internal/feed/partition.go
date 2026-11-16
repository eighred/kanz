package feed

import (
	"context"
	"hash/fnv"
	"sync"

	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
)

// Market-data hot-path throughput (PARITY-05a → PARITY-05b). The base Sink
// contract is a SINGLE synchronous Publish, so a whole vendor feed serializes
// through one downstream call and a single slow instrument stalls the entire
// stream. Partitioned decorates a Sink to fan the stream across N ordered
// LANES keyed by instrument_id: independent instruments publish in parallel
// (throughput), while every event for one instrument stays on ONE lane drained
// by ONE goroutine (per-instrument ordering is preserved end-to-end).
//
// # Backpressure without dropping or reordering
//
// Each lane is a BOUNDED buffered channel. Publish enqueues onto the event's
// lane and BLOCKS when that lane is full — the source (Bloomberg/Refinitiv/…)
// is throttled rather than any tick being dropped, and because a lane is a
// single-writer FIFO drained in enqueue order, ordering is never inverted.
// This is the synchronous-Publish backpressure contract of PARITY-01a, now
// applied per lane instead of globally.
//
// # Batching
//
// When the inner sink implements BatchSink, each lane greedily drains up to
// maxBatch already-queued events and forwards them in one PublishBatch call —
// amortizing the per-event publish cost (framing, network round-trip) that
// dominates p99 at production tick rates. A batch is always same-lane, so its
// events are contiguous and ordered for one instrument. A non-batch inner sink
// falls back to per-event Publish.
//
// # Error model
//
// Publishing is asynchronous, so a downstream failure cannot be returned from
// the Publish call that enqueued the failing event. Instead the first lane
// error is recorded stickily: subsequent Publish calls (and Close) return it,
// so the source stops promptly (fail-fast) while lanes keep draining so no
// enqueue deadlocks. Close stops the lanes and returns the first error seen.
type Partitioned struct {
	inner    Sink
	batch    BatchSink // non-nil iff inner implements BatchSink
	lanes    []chan *marketpb.MarketDataEvent
	maxBatch int

	wg      sync.WaitGroup
	closeMu sync.Mutex
	closed  bool

	errMu sync.Mutex
	err   error
}

// BatchSink is an optional Sink capability: forward a contiguous run of events
// in one call. Partitioned uses it to amortize per-event publish overhead; the
// composition root's bus-producer sink implements it over the producer's batch
// publish. Events passed to PublishBatch are always for a single lane.
type BatchSink interface {
	Sink
	PublishBatch(ctx context.Context, evs []*marketpb.MarketDataEvent) error
}

// DefaultLanes / DefaultLaneBuffer / DefaultMaxBatch are the hot-path defaults:
// enough lanes to parallelize across cores, a lane buffer deep enough to
// absorb bursts before backpressuring, and a batch cap that bounds tail
// latency while still amortizing publish overhead.
const (
	DefaultLanes      = 16
	DefaultLaneBuffer = 1024
	DefaultMaxBatch   = 64
)

// PartitionedOption customizes a Partitioned sink.
type PartitionedOption func(*partitionedCfg)

type partitionedCfg struct {
	lanes, buffer, maxBatch int
}

// WithLanes sets the number of parallel ordered lanes (≤0 ⇒ DefaultLanes).
func WithLanes(n int) PartitionedOption {
	return func(c *partitionedCfg) { c.lanes = n }
}

// WithLaneBuffer sets each lane's bounded buffer depth (≤0 ⇒ DefaultLaneBuffer).
// Smaller backpressures the source sooner; larger absorbs deeper bursts.
func WithLaneBuffer(n int) PartitionedOption {
	return func(c *partitionedCfg) { c.buffer = n }
}

// WithMaxBatch caps events coalesced into one PublishBatch (≤0 ⇒ DefaultMaxBatch;
// ignored when the inner sink is not a BatchSink).
func WithMaxBatch(n int) PartitionedOption {
	return func(c *partitionedCfg) { c.maxBatch = n }
}

// NewPartitioned wraps inner with per-instrument partitioning + batching and
// starts the lane workers. Call Close to drain and stop them.
func NewPartitioned(inner Sink, opts ...PartitionedOption) *Partitioned {
	cfg := partitionedCfg{lanes: DefaultLanes, buffer: DefaultLaneBuffer, maxBatch: DefaultMaxBatch}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.lanes <= 0 {
		cfg.lanes = DefaultLanes
	}
	if cfg.buffer <= 0 {
		cfg.buffer = DefaultLaneBuffer
	}
	if cfg.maxBatch <= 0 {
		cfg.maxBatch = DefaultMaxBatch
	}
	p := &Partitioned{inner: inner, maxBatch: cfg.maxBatch}
	if bs, ok := inner.(BatchSink); ok {
		p.batch = bs
	}
	p.lanes = make([]chan *marketpb.MarketDataEvent, cfg.lanes)
	for i := range p.lanes {
		ch := make(chan *marketpb.MarketDataEvent, cfg.buffer)
		p.lanes[i] = ch
		p.wg.Add(1)
		go p.worker(ch)
	}
	return p
}

// Publish routes ev to its instrument's lane, blocking when the lane is full
// (backpressure). Returns the sticky lane error if one has occurred, or a ctx
// error if ctx is cancelled while blocked. An event with an empty instrument_id
// is pinned to lane 0 (validation is the normalizer's job upstream).
func (p *Partitioned) Publish(ctx context.Context, ev *marketpb.MarketDataEvent) error {
	if err := p.sticky(); err != nil {
		return err
	}
	lane := p.lanes[laneFor(ev.GetInstrumentId(), len(p.lanes))]
	select {
	case lane <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// worker drains one lane in order, forwarding events (batched when possible)
// to the inner sink until the lane channel is closed.
func (p *Partitioned) worker(ch chan *marketpb.MarketDataEvent) {
	defer p.wg.Done()
	ctx := context.Background()
	buf := make([]*marketpb.MarketDataEvent, 0, p.maxBatch)
	for first := range ch {
		buf = append(buf[:0], first)
		// Greedily coalesce already-queued events for this lane (never blocks:
		// the default stops the run the instant the lane is momentarily empty).
	coalesce:
		for len(buf) < p.maxBatch {
			select {
			case ev, ok := <-ch:
				if !ok {
					break coalesce // channel closed+drained; flush what we have
				}
				buf = append(buf, ev)
			default:
				break coalesce
			}
		}
		p.forward(ctx, buf)
	}
}

// forward sends a same-lane run to the inner sink, batching when it can. A
// downstream error is recorded stickily; the worker keeps draining so Publish
// never deadlocks, and the source stops on the next Publish/Close.
func (p *Partitioned) forward(ctx context.Context, evs []*marketpb.MarketDataEvent) {
	if p.sticky() != nil {
		return // already failed — drop-drain to keep enqueues unblocked
	}
	if p.batch != nil {
		if err := p.batch.PublishBatch(ctx, evs); err != nil {
			p.setErr(err)
		}
		return
	}
	for _, ev := range evs {
		if err := p.inner.Publish(ctx, ev); err != nil {
			p.setErr(err)
			return
		}
	}
}

// Close stops accepting new events, drains all lanes, and returns the first
// lane error observed (nil on a clean drain). Idempotent.
func (p *Partitioned) Close() error {
	p.closeMu.Lock()
	if p.closed {
		p.closeMu.Unlock()
		p.wg.Wait()
		return p.sticky()
	}
	p.closed = true
	for _, ch := range p.lanes {
		close(ch)
	}
	p.closeMu.Unlock()
	p.wg.Wait()
	return p.sticky()
}

func (p *Partitioned) sticky() error {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	return p.err
}

func (p *Partitioned) setErr(err error) {
	if err == nil {
		return
	}
	p.errMu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.errMu.Unlock()
}

// laneFor maps an instrument id to a stable lane index so every event for one
// instrument is serialized on the same lane.
func laneFor(instrumentID string, lanes int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(instrumentID))
	return int(h.Sum32() % uint32(lanes))
}

var _ Sink = (*Partitioned)(nil)

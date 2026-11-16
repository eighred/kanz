package feed

import (
	"context"
	"fmt"
	"sync"
	"time"

	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
)

// Live data-quality gate (PARITY-01g). The Gate decorates a Sink and enforces
// quality on the live stream BEFORE an event reaches a downstream analytic:
//
//   - stale (event_time older than the freshness budget) ⇒ DROPPED. Deny-by-
//     default: a stale tick must not silently serve (the carried-forward
//     "a stale/failed feed must not serve" rule).
//   - out-of-order (event_time before this instrument's last) ⇒ DROPPED. A
//     regressing timestamp would corrupt any windowed/last-value read.
//   - source_sequence gap ⇒ REPORTED but PASSED. The tick itself is valid; a gap
//     means an EARLIER tick was lost upstream — dropping this one compounds the
//     loss rather than fixing it.
//
// Every breach is reported to onBreach, which the composition root maps to a
// DataException + the AUTO-01 observation FACT (the same break-detection →
// exception → controller path datamaster uses). The staleness check is that
// stance applied to a pre-publish market.v1 tick: event_time vs a wall-clock
// budget, on the tick itself rather than on its envelope.
type Gate struct {
	inner    Sink
	onBreach func(Breach)
	budget   time.Duration
	now      func() time.Time

	mu       sync.Mutex
	lastTime map[string]time.Time
	lastSeq  map[string]uint64
}

// BreachKind classifies a data-quality breach (the DataException kind the
// composition root raises).
type BreachKind string

const (
	BreachStale      BreachKind = "STALE"
	BreachOutOfOrder BreachKind = "OUT_OF_ORDER"
	BreachGap        BreachKind = "GAP"
)

// Breach is a detected data-quality fault on the live stream.
type Breach struct {
	Kind         BreachKind
	InstrumentID string
	Detail       string
	At           time.Time
}

// NewGate wraps inner with the DQ gate. budget ≤ 0 disables the staleness check.
// onBreach (nil ⇒ no-op) is the hook to the DataException/AUTO-01 emitter.
func NewGate(inner Sink, budget time.Duration, onBreach func(Breach)) *Gate {
	if onBreach == nil {
		onBreach = func(Breach) {}
	}
	return &Gate{
		inner: inner, onBreach: onBreach, budget: budget, now: time.Now,
		lastTime: map[string]time.Time{}, lastSeq: map[string]uint64{},
	}
}

// WithClock overrides the wall clock (tests pin a fixed now for staleness).
func (g *Gate) WithClock(now func() time.Time) *Gate { g.now = now; return g }

// Publish runs the DQ checks, drops stale/out-of-order ticks (reporting the
// breach), reports-but-passes a sequence gap, and forwards a clean tick to the
// inner sink.
func (g *Gate) Publish(ctx context.Context, ev *marketpb.MarketDataEvent) error {
	id := ev.GetInstrumentId()
	t := ev.GetEventTime().AsTime()
	seq := ev.GetSourceSequence()

	if g.budget > 0 {
		if lag := g.now().Sub(t); lag > g.budget {
			g.onBreach(Breach{BreachStale, id, fmt.Sprintf("event_time %s lags %s (budget %s)", t.UTC().Format(time.RFC3339Nano), lag.Truncate(time.Millisecond), g.budget), g.now()})
			return nil
		}
	}

	g.mu.Lock()
	if prev, ok := g.lastTime[id]; ok && t.Before(prev) {
		g.mu.Unlock()
		g.onBreach(Breach{BreachOutOfOrder, id, fmt.Sprintf("event_time %s precedes %s", t.UTC().Format(time.RFC3339Nano), prev.UTC().Format(time.RFC3339Nano)), g.now()})
		return nil
	}
	var gap *Breach
	if seq > 0 {
		if prevS, ok := g.lastSeq[id]; ok && seq != prevS+1 {
			gap = &Breach{BreachGap, id, fmt.Sprintf("source_sequence %d follows %d (expected %d)", seq, prevS, prevS+1), g.now()}
		}
		g.lastSeq[id] = seq
	}
	g.lastTime[id] = t
	g.mu.Unlock()

	if gap != nil {
		g.onBreach(*gap)
	}
	return g.inner.Publish(ctx, ev)
}

var _ Sink = (*Gate)(nil)

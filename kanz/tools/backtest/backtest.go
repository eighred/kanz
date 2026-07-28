// Package backtest is the strategy backtest harness (LAKE-01c). It drives a
// historical event stream — the EVT-20 replay Source — through a deterministic
// Strategy, exposing to each decision only the knowledge knowable at that
// event's event_time (the LAKE-01b point-in-time materializer pinned to an AsOf
// horizon). Because the strategy sees exactly what it saw live and computes its
// features with the same code the live engine uses, a backtest reproduces the
// live decision stream bit-for-bit (EVT-21d determinism); LAKE-01e asserts it.
//
// The harness is offline and single-threaded: it Collects the whole replay
// range into a deterministic order (event_time, then partition, then offset)
// before processing, so a run over the same range is reproducible regardless of
// the cross-partition arrival order Kafka gives (event-class-rules §1). A
// strategy MUST be deterministic — no wall-clock, no unseeded RNG — or the
// reproduction guarantee does not hold.
package backtest

import (
	"context"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/lake/dataset"
)

// Decision is one strategy output. It is the unit of the reproduction contract:
// two runs match iff their ordered Decision slices are equal.
type Decision struct {
	EventID      string    `json:"event_id"`
	InstrumentID string    `json:"instrument_id"`
	AsOf         time.Time `json:"as_of"`
	Action       string    `json:"action"`
	Quantity     float64   `json:"quantity"`
	Reason       string    `json:"reason"`
}

// Event is the decision input: one replayed envelope + payload.
type Event struct {
	Envelope *envelopepb.Envelope
	Payload  []byte
}

// PointInTime exposes only knowledge knowable at the event's event_time. The
// horizon is fixed to the event the strategy is reacting to, so a strategy
// cannot read the future no matter what it asks for.
type PointInTime interface {
	// AsOf is the knowledge horizon — the event's event_time.
	AsOf() time.Time
	// Features materializes the point-in-time feature row for an instrument as of
	// AsOf. Errors when the harness has no materializer wired.
	Features(ctx context.Context, instrumentID string) (dataset.Row, error)
}

// Strategy turns one event (+ point-in-time features) into zero or more
// decisions. It MUST be deterministic.
type Strategy interface {
	OnEvent(ctx context.Context, ev Event, pit PointInTime) ([]Decision, error)
}

// Result summarizes a run.
type Result struct {
	// Decisions are every decision the strategy emitted, in event order.
	Decisions []Decision
	// Events is the number of well-formed events processed.
	Events int
	// Malformed is the number of source frames that failed to unframe (skipped).
	Malformed int
}

// Harness runs a Strategy over a replay range.
type Harness struct {
	// Materializer supplies point-in-time features. nil ⇒ a strategy that calls
	// pit.Features gets an error (a price-only strategy needs none).
	Materializer *dataset.Materializer
}

// Run drains src into a deterministic order and folds each event through strat.
// The first strategy or source error aborts and is returned with the partial
// result.
func (h *Harness) Run(ctx context.Context, src Source, strat Strategy) (Result, error) {
	events, malformed, err := Collect(ctx, src)
	if err != nil {
		return Result{Malformed: malformed}, err
	}
	res := Result{Malformed: malformed}
	for _, ev := range events {
		asOf := ev.Envelope.GetEventTime().AsTime()
		pit := pointInTime{m: h.Materializer, asOf: asOf}
		ds, err := strat.OnEvent(ctx, Event{Envelope: ev.Envelope, Payload: ev.Payload}, pit)
		if err != nil {
			return res, err
		}
		res.Decisions = append(res.Decisions, ds...)
		res.Events++
	}
	return res, nil
}

// pointInTime pins a materializer to one knowledge horizon.
type pointInTime struct {
	m    *dataset.Materializer
	asOf time.Time
}

func (p pointInTime) AsOf() time.Time { return p.asOf }

func (p pointInTime) Features(ctx context.Context, instrumentID string) (dataset.Row, error) {
	if p.m == nil {
		return dataset.Row{}, errNoMaterializer
	}
	return p.m.Materialize(ctx, dataset.Sample{InstrumentID: instrumentID, AsOf: p.asOf})
}

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

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/alpha/outcome"
	"github.com/eighred/kanz/internal/alpha/score"
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

	// Score is the claim the strategy made about this decision, if it made one
	// (#416 C2). A VALUE, not a pointer, so Decision stays comparable — the
	// reproduction contract above is struct equality, and a pointer would compare
	// addresses and call two different claims identical.
	//
	// The zero value means "no claim", which is a real state: a rule-based
	// strategy forecasts nothing, and a CLOSE that flattens a position forecasts
	// nothing either.
	Score score.Score `json:"score,omitzero"`
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

	// Calibration grades the claims the strategy made, when a Resolver is wired
	// and at least one decision carried a score. nil otherwise.
	//
	// IT IS A SEPARATE QUESTION FROM WHETHER THE STRATEGY MADE MONEY, and both
	// are needed: a profitable strategy whose scores are wrong made money for a
	// reason it does not understand, and it will size the next trade on the same
	// misunderstanding.
	Calibration *score.Report

	// Unresolved is how many scored decisions could NOT be marked.
	//
	// REPORTED, not silently dropped. A run whose scores were 90% unresolved
	// produces a calibration report over the remaining tenth, and nothing else
	// would say so — the report would look thin rather than unrepresentative.
	Unresolved int

	// UnresolvedBy breaks Unresolved down by outcome.Reason.
	//
	// THE TOTAL ALONE IS NOT ACTIONABLE, and this comment used to be the whole
	// story ("the horizon had not elapsed [...] or the series had a gap") for a
	// single number that could not tell them apart. They are different problems
	// with different owners: horizon_open shrinks on its own as time passes,
	// incomplete_window is a market-data question, and
	// horizon_shorter_than_series is a model that will never be gradeable against
	// this series no matter how long anyone waits.
	UnresolvedBy map[outcome.Reason]int
}

// Harness runs a Strategy over a replay range.
type Harness struct {
	// Materializer supplies point-in-time features. nil ⇒ a strategy that calls
	// pit.Features gets an error (a price-only strategy needs none).
	Materializer *dataset.Materializer

	// Resolver marks each scored decision against the realized bar series. nil ⇒
	// no calibration is computed, and Result.Calibration is nil rather than an
	// empty report — "not measured" and "measured, and perfect" must not look the
	// same.
	Resolver *outcome.Resolver

	// CalibrationAsOf is the KNOWLEDGE horizon outcomes are marked at. Zero ⇒ now.
	//
	// Pinning it is what makes a calibration run reproducible: the bar store is
	// bitemporal, so an unpinned run silently produces different numbers next
	// month if a venue restated a candle, and nobody can say which run was right.
	CalibrationAsOf time.Time
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
	if err := h.calibrate(ctx, &res); err != nil {
		return res, err
	}
	return res, nil
}

// calibrate marks every scored decision and folds the outcomes into a report.
//
// A resolver error ABORTS rather than being counted as unresolved: it means the
// bar store is failing, and a calibration report built over whatever happened to
// read successfully would describe the outage rather than the strategy.
func (h *Harness) calibrate(ctx context.Context, res *Result) error {
	if h.Resolver == nil {
		return nil
	}
	var outs []score.Outcome
	for _, d := range res.Decisions {
		if d.Score.IsZero() {
			continue
		}
		o, reason, err := h.Resolver.Resolve(ctx, d.Score, d.InstrumentID, d.AsOf, h.CalibrationAsOf)
		if err != nil {
			return err
		}
		if !reason.OK() {
			res.Unresolved++
			if res.UnresolvedBy == nil {
				res.UnresolvedBy = map[outcome.Reason]int{}
			}
			res.UnresolvedBy[reason]++
			continue
		}
		outs = append(outs, o)
	}
	if len(outs) == 0 {
		// NO REPORT rather than an empty one. score.Calibrate refuses an empty set
		// for the same reason: a zero-valued Report reads as perfect calibration.
		return nil
	}
	rep, err := score.Calibrate(outs, score.DefaultBins)
	if err != nil {
		return err
	}
	res.Calibration = &rep
	return nil
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

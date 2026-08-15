package backtest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/alpha/outcome"
	"github.com/eighred/kanz/internal/alpha/score"
	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/tools/replay"
)

// A BACKTEST NOW GRADES THE STRATEGY'S CLAIMS, NOT ONLY ITS DECISIONS (#416 C2).
//
// Whether a strategy made money and whether its scores were honest are separate
// questions, and both matter: a profitable strategy whose claims are wrong made
// money for a reason it does not understand, and it will size the next trade on
// the same misunderstanding.

var t0 = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func cdec(f float64) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: int64(f * 100), Exponent: -2}
}

// barAt builds minute i with an explicit high, so a fixture can put the move
// exactly where it means to.
func barAt(i int, closePx, high float64) store.Bar {
	start := t0.Add(time.Duration(i) * time.Minute)
	return store.Bar{
		InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: store.Resolution1m,
		BucketStart: start,
		Open:        cdec(closePx), High: cdec(high), Low: cdec(closePx - 1), Close: cdec(closePx),
		Volume: cdec(1), KnowledgeTime: start.Add(time.Minute),
	}
}

// scoringStrategy emits one scored decision per event.
type scoringStrategy struct {
	probs []float64
	n     int
}

func (s *scoringStrategy) OnEvent(_ context.Context, ev Event, pit PointInTime) ([]Decision, error) {
	p := s.probs[s.n%len(s.probs)]
	s.n++
	sc, err := score.New(p, 0.05, 2*time.Minute, "test-model")
	if err != nil {
		return nil, err
	}
	return []Decision{{
		EventID: ev.Envelope.GetEventId(), InstrumentID: "BTC-USDT",
		AsOf: pit.AsOf(), Action: "BUY", Quantity: 1, Reason: "test", Score: sc,
	}}, nil
}

type unscored struct{}

func (unscored) OnEvent(_ context.Context, ev Event, pit PointInTime) ([]Decision, error) {
	return []Decision{{EventID: ev.Envelope.GetEventId(), InstrumentID: "BTC-USDT",
		AsOf: pit.AsOf(), Action: "BUY", Quantity: 1}}, nil
}

func eventsAt(mins ...int) []replay.Event {
	out := make([]replay.Event, 0, len(mins))
	for i, m := range mins {
		out = append(out, replay.Event{Envelope: &envelopepb.Envelope{
			EventId:   fmt.Sprintf("ev-%d", i),
			EventTime: timestamppb.New(t0.Add(time.Duration(m) * time.Minute)),
		}})
	}
	return out
}

func barStore(t *testing.T, bars ...store.Bar) *store.Memory {
	t.Helper()
	m := store.NewMemory()
	if err := m.PutBars(context.Background(), bars); err != nil {
		t.Fatal(err)
	}
	return m
}

func newResolver(t *testing.T, m store.BarStore) *outcome.Resolver {
	t.Helper()
	r, err := outcome.NewResolver(m, outcome.Config{Venue: "XBIN"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// WITH NO RESOLVER THERE IS NO REPORT — not an empty one.
//
// "Not measured" and "measured, and perfect" must not look the same, and a
// zero-valued Report is indistinguishable from flawless calibration.
func TestRun_WithoutAResolverThereIsNoCalibration(t *testing.T) {
	h := &Harness{}
	res, err := h.Run(context.Background(), NewSliceSource(eventsAt(1, 2)),
		&scoringStrategy{probs: []float64{0.7}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Calibration != nil {
		t.Error("a harness with no resolver produced a calibration report — an unmeasured run " +
			"must not be reportable as a measured one")
	}
	if res.Unresolved != 0 {
		t.Errorf("Unresolved = %d with no resolver, want 0", res.Unresolved)
	}
}

// A SCORED RUN IS GRADED, AND THE OVERCONFIDENCE SHOWS UP AS A NEGATIVE SKILL.
func TestRun_ScoredDecisionsAreCalibrated(t *testing.T) {
	// Flat at 100 with a spike to 106 in minute 3 only: a +5% claim made at
	// minute 2 hits, one made at minute 5 does not.
	m := barStore(t,
		barAt(1, 100, 100), barAt(2, 100, 100), barAt(3, 100, 106),
		barAt(4, 100, 100), barAt(5, 100, 100), barAt(6, 100, 100), barAt(7, 100, 100),
	)
	h := &Harness{Resolver: newResolver(t, m), CalibrationAsOf: t0.Add(2 * time.Hour)}

	res, err := h.Run(context.Background(), NewSliceSource(eventsAt(2, 5)),
		&scoringStrategy{probs: []float64{0.9, 0.9}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Calibration == nil {
		t.Fatalf("no calibration report over two scored decisions (Unresolved=%d)", res.Unresolved)
	}
	if res.Calibration.N != 2 {
		t.Fatalf("N = %d, want 2 (Unresolved=%d)", res.Calibration.N, res.Unresolved)
	}
	if res.Calibration.BaseRate != 0.5 {
		t.Errorf("BaseRate = %v, want 0.5 — one claim of the two came true", res.Calibration.BaseRate)
	}
	// It asserted 0.9 twice and was right half the time. The sign is what says so.
	if res.Calibration.BrierSkill >= 0 {
		t.Errorf("BrierSkill = %v, want negative — 0.9 asserted against a 0.5 outcome",
			res.Calibration.BrierSkill)
	}
}

// AN UNRESOLVABLE DECISION IS COUNTED, NOT SILENTLY DROPPED.
//
// A run whose scores were mostly unresolved yields a report over the remainder,
// and nothing else would say so — it would look thin rather than
// unrepresentative.
func TestRun_UnresolvedDecisionsAreCounted(t *testing.T) {
	m := barStore(t, barAt(1, 100, 100), barAt(2, 100, 100), barAt(3, 100, 100), barAt(4, 100, 100))
	// The knowledge horizon sits before the second decision's window closes.
	h := &Harness{Resolver: newResolver(t, m), CalibrationAsOf: t0.Add(5 * time.Minute)}

	res, err := h.Run(context.Background(), NewSliceSource(eventsAt(2, 30)),
		&scoringStrategy{probs: []float64{0.7, 0.7}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Unresolved != 1 {
		t.Errorf("Unresolved = %d, want 1 — the minute-30 claim's horizon ends past the "+
			"knowledge horizon and cannot be marked", res.Unresolved)
	}
}

// AN UNSCORED STRATEGY IS NOT GRADED, AND THAT IS NOT AN ERROR.
//
// A rule-based strategy makes no probabilistic claim; calibrating it would mean
// inventing one.
func TestRun_AnUnscoredStrategyProducesNoReport(t *testing.T) {
	m := barStore(t, barAt(1, 100, 100), barAt(2, 100, 100), barAt(3, 100, 100))
	h := &Harness{Resolver: newResolver(t, m), CalibrationAsOf: t0.Add(2 * time.Hour)}

	res, err := h.Run(context.Background(), NewSliceSource(eventsAt(2)), unscored{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Calibration != nil {
		t.Error("an unscored strategy was given a calibration report")
	}
	if res.Unresolved != 0 {
		t.Errorf("Unresolved = %d — a decision with no claim is not an unresolved claim",
			res.Unresolved)
	}
}

// THE CLAIM SURVIVES THE JSON REPRODUCTION RECORD.
//
// Decision's contract is that two runs match iff their ordered slices are equal.
// A Score whose fields are unexported would serialise as {} and make two
// different claims compare identical — so it carries its own JSON form.
func TestDecision_ScoreRoundTripsThroughJSON(t *testing.T) {
	sc, err := score.New(0.61, 0.005, 90*time.Minute, "m-7")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(Decision{
		EventID: "e1", InstrumentID: "BTC-USDT", AsOf: t0, Action: "BUY", Score: sc})
	if err != nil {
		t.Fatal(err)
	}
	var back Decision
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal %s: %v", blob, err)
	}
	if back.Score != sc {
		t.Errorf("the claim changed through JSON: %v -> %v (%s)", sc, back.Score, blob)
	}

	// AND AN UNSCORED DECISION ROUND-TRIPS AS UNSCORED, rather than arriving as a
	// claim of probability zero — which is a confident short, not silence.
	blob2, err := json.Marshal(Decision{EventID: "e2"})
	if err != nil {
		t.Fatal(err)
	}
	var plain Decision
	if err := json.Unmarshal(blob2, &plain); err != nil {
		t.Fatalf("unmarshal %s: %v", blob2, err)
	}
	if !plain.Score.IsZero() {
		t.Errorf("an unscored decision came back carrying %v", plain.Score)
	}
}

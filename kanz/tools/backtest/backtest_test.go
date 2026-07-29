package backtest_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/lake/dataset"
	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/tools/backtest"
	"github.com/eighred/kanz/tools/replay"
)

var day0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func day(i int) time.Time { return day0.AddDate(0, 0, i) }

// volStrategy is a deterministic toy strategy: REDUCE when point-in-time
// volatility exceeds a threshold, else HOLD; no decision until there is enough
// history to estimate vol. It reads features through pit, so it can only see
// what was knowable at the event's event_time.
type volStrategy struct{ threshold float64 }

func (s volStrategy) OnEvent(ctx context.Context, ev backtest.Event, pit backtest.PointInTime) ([]backtest.Decision, error) {
	inst := ev.Envelope.GetPartitionKey()
	row, err := pit.Features(ctx, inst)
	if err != nil {
		return nil, err
	}
	vol, ok := row.Features["vol"]
	if !ok {
		return nil, nil
	}
	action := "HOLD"
	if vol > s.threshold {
		action = "REDUCE"
	}
	return []backtest.Decision{{
		EventID:      ev.Envelope.GetEventId(),
		InstrumentID: inst,
		AsOf:         pit.AsOf(),
		Action:       action,
		Quantity:     vol,
		Reason:       "vol-threshold",
	}}, nil
}

func event(inst, id string, dayIdx, partition, offset int) replay.Event {
	return replay.Event{
		Envelope: &envelopepb.Envelope{
			EventId:      id,
			Domain:       "market",
			EventType:    "market.equity.trade",
			PartitionKey: inst,
			EventTime:    timestamppb.New(day(dayIdx)),
		},
		Partition: partition,
		Offset:    int64(offset),
	}
}

func seed(t *testing.T, s store.Store, inst string) {
	t.Helper()
	for i := 1; i <= 30; i++ {
		// A noisy ramp so volatility is non-trivial and a correction can move it.
		cents := int64(10000 + i*100)
		if i%2 == 0 {
			cents += 300
		}
		if err := s.Put(context.Background(), []store.Observation{{
			InstrumentID:    inst,
			ObservationTime: day(i),
			Price:           &commonpb.Decimal{Coefficient: cents, Exponent: -2},
			Kind:            store.PriceKindClose,
			KnowledgeTime:   day(i),
		}}); err != nil {
			t.Fatal(err)
		}
	}
}

// tradingDays builds one event per day over [22,30] — past the 20-return floor,
// so the strategy actually decides.
func tradingDays(inst string) []replay.Event {
	var evs []replay.Event
	for d := 22; d <= 30; d++ {
		evs = append(evs, event(inst, fmt.Sprintf("%s-%d", inst, d), d, 0, d))
	}
	return evs
}

func harness(s store.Store) *backtest.Harness {
	return &backtest.Harness{Materializer: dataset.NewMaterializer(s, nil, dataset.Config{})}
}

func TestRunIsDeterministic(t *testing.T) {
	s := store.NewMemory()
	seed(t, s, "AAPL")
	h := harness(s)
	strat := volStrategy{threshold: 0.01}

	r1, err := h.Run(context.Background(), backtest.NewSliceSource(tradingDays("AAPL")), strat)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := h.Run(context.Background(), backtest.NewSliceSource(tradingDays("AAPL")), strat)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Decisions) == 0 {
		t.Fatal("no decisions produced — test would be vacuous")
	}
	if !reflect.DeepEqual(r1.Decisions, r2.Decisions) {
		t.Errorf("non-deterministic: run1=%v run2=%v", r1.Decisions, r2.Decisions)
	}
}

func TestCollectImposesDeterministicOrder(t *testing.T) {
	s := store.NewMemory()
	seed(t, s, "AAPL")
	h := harness(s)
	strat := volStrategy{threshold: 0.01}

	inOrder := tradingDays("AAPL")
	shuffled := []replay.Event{inOrder[5], inOrder[0], inOrder[8], inOrder[2], inOrder[1], inOrder[7], inOrder[3], inOrder[6], inOrder[4]}

	ordered, err := h.Run(context.Background(), backtest.NewSliceSource(inOrder), strat)
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.Run(context.Background(), backtest.NewSliceSource(shuffled), strat)
	if err != nil {
		t.Fatal(err)
	}
	// Collect sorts by event_time, so a shuffled source yields the same stream.
	if !reflect.DeepEqual(ordered.Decisions, out.Decisions) {
		t.Errorf("ordering not deterministic:\n ordered=%v\n shuffled=%v", ordered.Decisions, out.Decisions)
	}
}

// The headline LAKE-01e property: a backtest over historical events reproduces
// the live decision stream bit-for-bit, because every feature read is scoped to
// the event's event_time and a correction that arrived later cannot leak in.
func TestBacktestReproducesLiveDecisions(t *testing.T) {
	ctx := context.Background()
	strat := volStrategy{threshold: 0.01}
	events := tradingDays("AAPL")

	// Live: the store as it existed while events streamed (no late correction).
	live := store.NewMemory()
	seed(t, live, "AAPL")
	liveRes, err := harness(live).Run(ctx, backtest.NewSliceSource(events), strat)
	if err != nil {
		t.Fatal(err)
	}
	if len(liveRes.Decisions) == 0 {
		t.Fatal("no live decisions — test would be vacuous")
	}

	// Backtest: same series, but a day-5 restatement learned on day 40 — AFTER
	// every event's event_time (≤ day 30). A point-in-time read must hide it.
	bt := store.NewMemory()
	seed(t, bt, "AAPL")
	if err := bt.Put(ctx, []store.Observation{{
		InstrumentID:    "AAPL",
		ObservationTime: day(5),
		Price:           &commonpb.Decimal{Coefficient: 99999, Exponent: -2}, // wildly different
		Kind:            store.PriceKindClose,
		KnowledgeTime:   day(40),
	}}); err != nil {
		t.Fatal(err)
	}
	btRes, err := harness(bt).Run(ctx, backtest.NewSliceSource(events), strat)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(liveRes.Decisions, btRes.Decisions) {
		t.Errorf("backtest did not reproduce live decisions (future leakage)\n live=%v\n bt  =%v",
			liveRes.Decisions, btRes.Decisions)
	}
}

// Contrast: if reads were NOT point-in-time (a naive "latest value wins" store),
// the day-40 correction would leak into a backtest dated within its knowledge,
// changing decisions. We prove the seam matters by reading the same instrument
// at an as-of past the correction and showing the feature differs.
func TestCorrectionVisibleOnlyAfterKnowledgeTime(t *testing.T) {
	ctx := context.Background()
	s := store.NewMemory()
	seed(t, s, "AAPL")
	m := dataset.NewMaterializer(s, nil, dataset.Config{})

	before, _ := m.Materialize(ctx, dataset.Sample{InstrumentID: "AAPL", AsOf: day(30)})
	if err := s.Put(ctx, []store.Observation{{
		InstrumentID:    "AAPL",
		ObservationTime: day(5),
		Price:           &commonpb.Decimal{Coefficient: 99999, Exponent: -2},
		Kind:            store.PriceKindClose,
		KnowledgeTime:   day(40),
	}}); err != nil {
		t.Fatal(err)
	}
	asOf30, _ := m.Materialize(ctx, dataset.Sample{InstrumentID: "AAPL", AsOf: day(30)})
	asOf50, _ := m.Materialize(ctx, dataset.Sample{InstrumentID: "AAPL", AsOf: day(50)})

	if !reflect.DeepEqual(before.Features, asOf30.Features) {
		t.Error("as-of day 30 changed though the correction is known only on day 40 (leak)")
	}
	if reflect.DeepEqual(before.Features, asOf50.Features) {
		t.Error("as-of day 50 should reflect the day-40 correction, but it did not")
	}
}

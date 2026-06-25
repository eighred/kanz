package backtest

import (
	"context"
	"errors"
	"io"
	"sort"

	"github.com/kanz-eng/kanz/tools/replay"
)

// Source is the harness upstream — the EVT-20 replay Source (Reader, or an
// in-memory fake). Reusing it directly means a backtest reads the same durable
// log a replay run republishes from.
type Source = replay.Source

var errNoMaterializer = errors.New("backtest: no materializer wired; strategy requested point-in-time features")

// Collect drains a Source fully and returns its events in a deterministic order
// — by envelope event_time, then Kafka partition, then offset. Cross-partition
// arrival order is non-deterministic (Kafka gives no global order,
// event-class-rules §1), so a reproducible backtest must impose one; domain
// event_time is the honest axis, with (partition, offset) as a stable
// tie-breaker. Malformed frames are skipped and counted (mirroring the replay
// Pipeline), not fatal.
func Collect(ctx context.Context, src Source) (events []replay.Event, malformed int, err error) {
	for {
		ev, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var mfe *replay.MalformedFrameError
			if errors.As(err, &mfe) {
				malformed++
				continue
			}
			return nil, malformed, err
		}
		if ev.Envelope == nil {
			return nil, malformed, errors.New("backtest: source returned nil envelope on success path")
		}
		events = append(events, ev)
	}
	sort.SliceStable(events, func(i, j int) bool {
		ti := events[i].Envelope.GetEventTime().AsTime()
		tj := events[j].Envelope.GetEventTime().AsTime()
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		if events[i].Partition != events[j].Partition {
			return events[i].Partition < events[j].Partition
		}
		return events[i].Offset < events[j].Offset
	})
	return events, malformed, nil
}

// SliceSource is an in-memory Source over a fixed event slice — for offline runs
// and tests, where the input order is controlled and reproducible.
type SliceSource struct {
	events []replay.Event
	i      int
}

func NewSliceSource(events []replay.Event) *SliceSource {
	return &SliceSource{events: events}
}

func (s *SliceSource) Next(_ context.Context) (replay.Event, error) {
	if s.i >= len(s.events) {
		return replay.Event{}, io.EOF
	}
	ev := s.events[s.i]
	s.i++
	return ev, nil
}

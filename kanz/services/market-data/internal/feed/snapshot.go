package feed

import (
	"context"
	"sort"
	"sync"

	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
)

// Latest-quote snapshot (PARITY-01h). A calibration model (PARITY-03: the rate
// curve from deposit/swap/futures quotes, the vol surface from listed options,
// the credit curve from CDS) does not consume the live tick STREAM — it needs
// the LATEST quote for each calibration instrument as of now. Snapshot is a Sink
// that maintains exactly that: the most-recent event per instrument, queryable
// point-in-time. The composition root tees the normalized stream into the bus
// producer AND this snapshot (see Tee); the calibrators read Latest/All.
type Snapshot struct {
	mu     sync.RWMutex
	latest map[string]*marketpb.MarketDataEvent
}

// NewSnapshot returns an empty snapshot.
func NewSnapshot() *Snapshot {
	return &Snapshot{latest: map[string]*marketpb.MarketDataEvent{}}
}

// Publish records the event as the latest for its instrument. It never errors
// (a snapshot cannot backpressure — it is a last-value cache), so teeing it
// alongside a real sink never blocks the stream on the snapshot.
func (s *Snapshot) Publish(_ context.Context, ev *marketpb.MarketDataEvent) error {
	s.mu.Lock()
	s.latest[ev.GetInstrumentId()] = ev
	s.mu.Unlock()
	return nil
}

// Latest returns the most-recent event for an instrument, or ok=false if none
// has been seen.
func (s *Snapshot) Latest(instrumentID string) (*marketpb.MarketDataEvent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ev, ok := s.latest[instrumentID]
	return ev, ok
}

// Instruments returns the instruments with a latest value, sorted — the
// calibrator's input universe.
func (s *Snapshot) Instruments() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.latest))
	for id := range s.latest {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

var _ Sink = (*Snapshot)(nil)

// Tee fans an event out to every sink in order, stopping at (and returning) the
// first sink error — so a backpressuring/failing real sink still stalls the
// stream while a pure-cache sink (Snapshot) never does. The composition root
// uses Tee to feed both the bus producer and the calibration snapshot from one
// adapter.
func Tee(sinks ...Sink) Sink { return teeSink(sinks) }

type teeSink []Sink

func (t teeSink) Publish(ctx context.Context, ev *marketpb.MarketDataEvent) error {
	for _, s := range t {
		if err := s.Publish(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

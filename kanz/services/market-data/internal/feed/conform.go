package feed

import (
	"context"
	"fmt"
	"sync"
	"time"

	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
)

// Conform runs an adapter against a subscription and checks the emitted stream
// against the canonical invariants every market-data source must satisfy:
//
//   - normalization: every event passes Validate (required fields + a data variant);
//   - scope: only subscribed instruments are emitted;
//   - ordering: per-instrument event_time is non-decreasing;
//   - gaps: per-instrument source_sequence has no break;
//   - fidelity: the emitted stream equals want (the golden expected output).
//
// It is the conformance harness PARITY-01b-d must pass — a real vendor adapter
// is fed a captured session and its normalized output is checked here. Returns
// the list of violations (empty ⇒ conformant). The adapter is run with a bounded
// deadline so a non-terminating Run cannot hang the check.
func Conform(a Adapter, instruments []string, want Session) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sink := &captureSink{}
	runErr := a.Run(ctx, instruments, sink)

	var v []string
	if runErr != nil {
		v = append(v, fmt.Sprintf("Run returned error: %v", runErr))
	}
	got := sink.events()

	subscribed := make(map[string]struct{}, len(instruments))
	for _, id := range instruments {
		subscribed[id] = struct{}{}
	}
	for i, ev := range got {
		if err := Validate(ev); err != nil {
			v = append(v, fmt.Sprintf("event %d invalid: %v", i, err))
		}
		if len(subscribed) > 0 {
			if _, ok := subscribed[ev.GetInstrumentId()]; !ok {
				v = append(v, fmt.Sprintf("event %d for unsubscribed instrument %q", i, ev.GetInstrumentId()))
			}
		}
	}
	v = append(v, OutOfOrder(got)...)
	v = append(v, Gap(got)...)

	if len(got) != len(want) {
		v = append(v, fmt.Sprintf("emitted %d events, want %d", len(got), len(want)))
		return v
	}
	for i := range want {
		if !sameEvent(got[i], want[i]) {
			v = append(v, fmt.Sprintf("event %d mismatch: got %s/%d want %s/%d", i,
				got[i].GetInstrumentId(), got[i].GetSourceSequence(),
				want[i].GetInstrumentId(), want[i].GetSourceSequence()))
		}
	}
	return v
}

// sameEvent compares the identity + sequencing of two normalized events (the
// fields the harness asserts fidelity on — not a deep proto equality, which is
// the builders' concern).
func sameEvent(a, b *marketpb.MarketDataEvent) bool {
	return a.GetInstrumentId() == b.GetInstrumentId() &&
		a.GetSourceSequence() == b.GetSourceSequence() &&
		a.GetEventTime().AsTime().Equal(b.GetEventTime().AsTime())
}

// captureSink records the emitted stream in order — the harness's bounded sink.
type captureSink struct {
	mu  sync.Mutex
	evs []*marketpb.MarketDataEvent
}

func (c *captureSink) Publish(_ context.Context, ev *marketpb.MarketDataEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evs = append(c.evs, ev)
	return nil
}

func (c *captureSink) events() []*marketpb.MarketDataEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*marketpb.MarketDataEvent, len(c.evs))
	copy(out, c.evs)
	return out
}

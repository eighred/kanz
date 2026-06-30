package feed

import (
	"context"
	"time"

	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
)

// Session is a recorded, ordered run of market events — a "golden session" the
// conformance harness replays and a SimAdapter serves. A captured vendor session
// (the expected normalized output) is the fixture every concrete adapter is
// tested against.
type Session []*marketpb.MarketDataEvent

// SimAdapter is the dependency-free default Adapter: it replays a Session to the
// sink, filtered to the subscribed instruments, in order. Publish is synchronous
// (so a slow sink backpressures the replay, exercising the same contract a live
// adapter must honor). It returns when the session is exhausted or ctx is
// canceled — deterministic, so it drives the whole ingestion path in tests with
// no vendor connection.
type SimAdapter struct {
	Name    string
	Session Session
	// Loop replays the session forever (until ctx) when set — useful for a local
	// boot that wants a continuous synthetic feed.
	Loop bool
}

// Vendor returns the sim source name.
func (a *SimAdapter) Vendor() string {
	if a.Name == "" {
		return "SIM"
	}
	return a.Name
}

// Run replays the session to sink, delivering only events for subscribed
// instruments (empty ⇒ all). A sink error aborts the run and is returned (the
// adapter does not retry a sink failure — that is the sink's/composition root's
// policy). ctx cancellation returns nil.
func (a *SimAdapter) Run(ctx context.Context, instruments []string, sink Sink) error {
	want := make(map[string]struct{}, len(instruments))
	for _, id := range instruments {
		want[id] = struct{}{}
	}
	for {
		for _, ev := range a.Session {
			if err := ctx.Err(); err != nil {
				return nil
			}
			if len(want) > 0 {
				if _, ok := want[ev.GetInstrumentId()]; !ok {
					continue
				}
			}
			if err := sink.Publish(ctx, ev); err != nil {
				return err
			}
		}
		if !a.Loop {
			return nil
		}
	}
}

var _ Adapter = (*SimAdapter)(nil)

// Reconnect runs connect (one connect-and-stream attempt) repeatedly with
// exponential backoff until ctx is canceled or connect returns nil (a clean
// shutdown). A live adapter wraps its session loop in this so a transient vendor
// disconnect is retried rather than ending the Run — the "reconnects internally"
// half of the Adapter contract. backoff doubles from base to max; a successful
// connect (returning a transient error after streaming for a while) is retried
// from base via the reset signaled by ctx-aware sleeping. Returns ctx.Err() on
// cancellation, or nil when connect returns nil.
func Reconnect(ctx context.Context, base, max time.Duration, connect func(context.Context) error) error {
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	if max < base {
		max = 30 * time.Second
	}
	delay := base
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := connect(ctx)
		if err == nil {
			return nil // clean shutdown
		}
		// transient — back off, then retry (capped).
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		if delay < max {
			delay *= 2
			if delay > max {
				delay = max
			}
		}
	}
}

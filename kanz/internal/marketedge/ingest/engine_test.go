package ingest

import (
	"context"
	"io"
	"math/big"
	"sync"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"

	"github.com/kanz-eng/kanz/internal/dec"
	"github.com/kanz-eng/kanz/internal/marketedge/book"
	"github.com/kanz-eng/kanz/internal/marketedge/depth"
	"github.com/kanz-eng/kanz/pkg/bus"
)

type capture struct {
	mu     sync.Mutex
	events []bus.Event
}

func (c *capture) Publish(_ context.Context, e bus.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return nil
}

func (c *capture) last() (bus.Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) == 0 {
		return bus.Event{}, false
	}
	return c.events[len(c.events)-1], true
}

// scriptedSource replays a fixed list of updates, then blocks until ctx done.
type scriptedSource struct {
	updates []depth.Update
	i       int
}

func (s *scriptedSource) Recv(ctx context.Context) (depth.Update, error) {
	if s.i < len(s.updates) {
		u := s.updates[s.i]
		s.i++
		return u, nil
	}
	<-ctx.Done()
	return depth.Update{}, ctx.Err()
}

func d(v string) *commonpb.Decimal {
	r, _ := new(big.Rat).SetString(v)
	return dec.ToProto(r)
}

func lvl(p, s string) *marketpb.PriceLevel {
	return &marketpb.PriceLevel{Price: d(p), Size: d(s)}
}

func TestEngine_FoldsAndPublishesSnapshot(t *testing.T) {
	src := &scriptedSource{updates: []depth.Update{
		{Snapshot: &marketpb.OrderBookSnapshot{
			InstrumentId: "BTC-USD", LastUpdateSequence: 1,
			Bids: []*marketpb.PriceLevel{lvl("50000", "1")},
			Asks: []*marketpb.PriceLevel{lvl("50001", "1")},
		}},
		{Delta: &marketpb.OrderBookDelta{
			InstrumentId: "BTC-USD", PrevUpdateSequence: 1, LastUpdateSequence: 2,
			Bids: []*marketpb.PriceLevel{lvl("50000", "4")},
		}},
	}}
	cap := &capture{}
	eng := New(Config{
		Book: book.New("BTC-USD", "BTCUSDT", "BINANCE"), Source: src, Publisher: cap,
		SnapshotInterval: 20 * time.Millisecond, SnapshotDepth: 10,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go func() { _ = eng.Run(ctx) }()

	// Wait for a published snapshot reflecting the folded delta.
	deadline := time.After(400 * time.Millisecond)
	for {
		if ev, ok := cap.last(); ok {
			snap, _ := ev.Payload.(*marketpb.OrderBookSnapshot)
			if snap != nil && snap.GetLastUpdateSequence() == 2 {
				if ev.Subject != SubjectBookSnapshot {
					t.Fatalf("subject = %q, want %q", ev.Subject, SubjectBookSnapshot)
				}
				if got := dec.FromProto(snap.GetBids()[0].GetSize()); got.Cmp(big.NewRat(4, 1)) != 0 {
					t.Fatalf("published best bid size = %s, want 4 (delta folded)", got.RatString())
				}
				return
			}
		}
		select {
		case <-deadline:
			t.Fatal("no snapshot reflecting seq 2 was published in time")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestEngine_EmptyBookNotPublished(t *testing.T) {
	cap := &capture{}
	eng := New(Config{
		Book: book.New("ETH-USD", "ETHUSDT", "SIM"), Source: &scriptedSource{}, Publisher: cap,
		SnapshotInterval: 10 * time.Millisecond, SnapshotDepth: 10,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_ = eng.Run(ctx)
	if _, ok := cap.last(); ok {
		t.Fatal("an empty book must not publish a snapshot")
	}
}

func TestEngine_SourceErrorStopsRun(t *testing.T) {
	eng := New(Config{
		Book: book.New("BTC-USD", "BTCUSDT", "SIM"), Source: &errSource{}, Publisher: &capture{},
		SnapshotInterval: time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := eng.Run(ctx); err != io.ErrUnexpectedEOF {
		t.Fatalf("Run err = %v, want ErrUnexpectedEOF (fatal source error surfaces)", err)
	}
}

type errSource struct{}

func (errSource) Recv(context.Context) (depth.Update, error) {
	return depth.Update{}, io.ErrUnexpectedEOF
}

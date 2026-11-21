package ingest

import (
	"context"
	"errors"
	"io"
	"math/big"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketedge/book"
	"github.com/eighred/kanz/internal/marketedge/depth"
	"github.com/eighred/kanz/pkg/bus"
)

type capture struct {
	mu     sync.Mutex
	events []bus.Event
}

// Publish enforces the tenant rule the real bus.Producer enforces. Snapshots are
// published off a ticker with no inbound envelope, so nothing supplies a tenant
// but Config.Tenant (or the caller's ProducerConfig.Tenant, which a library
// cannot see). A double that accepted an untenanted snapshot would let this
// engine's correctness silently depend on how each caller wires its producer —
// which is exactly what it did before Config.Tenant existed.
func (c *capture) Publish(ctx context.Context, e bus.Event) error {
	tenant := e.TenantID
	if tenant == "" {
		tenant = bus.TenantIDFromContext(ctx)
	}
	if tenant == "" {
		return errors.New("envelope validation: tenant_id required")
	}
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

// lastOn returns the newest captured event on one subject.
//
// THE ENGINE PUBLISHES TWO SUBJECTS PER TICK SINCE #876 - the book snapshot and
// the top-of-book quote derived from it - so `last()` alone no longer answers
// "the newest snapshot". It answers "the newest event", which is the quote,
// whose payload is a MarketDataEvent and not an OrderBookSnapshot. A test
// type-asserting the payload would simply never match and would fail on its
// deadline with a message about the snapshot, which is the wrong diagnosis.
func (c *capture) lastOn(subject string) (bus.Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.events) - 1; i >= 0; i-- {
		if c.events[i].Subject == subject {
			return c.events[i], true
		}
	}
	return bus.Event{}, false
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

type updateFeed chan depth.Update

func (feed updateFeed) Recv(ctx context.Context) (depth.Update, error) {
	select {
	case u := <-feed:
		return u, nil
	case <-ctx.Done():
		return depth.Update{}, ctx.Err()
	}
}

type retryOncePublisher struct {
	capture
	failSubject string
}

func (p *retryOncePublisher) Publish(ctx context.Context, e bus.Event) error {
	if p.failSubject == e.Subject {
		p.failSubject = ""
		return errors.New("temporary publish failure")
	}
	return p.capture.Publish(ctx, e)
}

func TestOnChangeCoalescesFoldsAndRetriesOnlyTheFailedFact(t *testing.T) {
	for _, failed := range []string{SubjectBookSnapshot, SubjectMarketCryptoQuote} {
		t.Run(failed, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				feed := make(updateFeed)
				pub := &retryOncePublisher{failSubject: failed}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				eng := New(Config{Book: book.New("BTC-USDT", "BTCUSDT", "BINANCE"), Source: feed, Publisher: pub,
					Tenant: "pilot", SnapshotInterval: time.Second, SnapshotOnChange: true})
				done := make(chan error, 1)
				go func() { done <- eng.Run(ctx) }()
				step := func(d time.Duration) { time.Sleep(d); synctest.Wait() }
				snapshot := func(seq uint64) {
					feed <- depth.Update{Snapshot: &marketpb.OrderBookSnapshot{LastUpdateSequence: seq,
						EventTime: tsOf(time.Now()), Bids: []*marketpb.PriceLevel{lvl("10", "2")}, Asks: []*marketpb.PriceLevel{lvl("11", "3")}}}
					synctest.Wait()
				}
				count := func(want int) {
					t.Helper()
					if len(pub.events) != want {
						t.Fatalf("published %d facts, want %d", len(pub.events), want)
					}
				}
				snapshot(1)
				count(1) // one leg succeeded without waiting for a periodic tick
				step(time.Second - time.Nanosecond)
				count(1)
				step(time.Nanosecond)
				count(2)
				if pub.events[0].Subject == pub.events[1].Subject {
					t.Fatal("retried an already published fact")
				}
				snapshot(2)
				snapshot(3)
				step(time.Second - time.Nanosecond)
				count(2)
				step(time.Nanosecond)
				count(4)
				got := pub.events[2].Payload.(*marketpb.OrderBookSnapshot)
				if got.LastUpdateSequence != 3 {
					t.Fatalf("published sequence %d", got.LastUpdateSequence)
				}
				step(3 * time.Second)
				count(4)
				feed <- depth.Update{Delta: &marketpb.OrderBookDelta{PrevUpdateSequence: 8, LastUpdateSequence: 9}}
				synctest.Wait()
				count(4)
				snapshot(4)
				count(6) // first valid change after idle publishes immediately
				snapshot(5)
				cancel()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				count(6)
			})
		})
	}
}

func TestEngine_FoldsAndPublishesSnapshot(t *testing.T) {
	src := &scriptedSource{updates: []depth.Update{
		{Snapshot: &marketpb.OrderBookSnapshot{
			InstrumentId: "BTC-USD", LastUpdateSequence: 1,
			Bids: []*marketpb.PriceLevel{lvl("50000", "1")},
			Asks: []*marketpb.PriceLevel{lvl("50001", "1")},
			// Not optional on the wire, so not optional here: since #957 the fold
			// REFUSES a snapshot with no venue time rather than stamping it 1970.
			EventTime: tsOf(time.Now().UTC()),
		}},
		{Delta: &marketpb.OrderBookDelta{
			InstrumentId: "BTC-USD", PrevUpdateSequence: 1, LastUpdateSequence: 2,
			Bids: []*marketpb.PriceLevel{lvl("50000", "4")},
		}},
	}}
	cap := &capture{}
	eng := New(Config{
		Book: book.New("BTC-USD", "BTCUSDT", "BINANCE"), Source: src, Publisher: cap,
		SnapshotInterval: 20 * time.Millisecond, SnapshotDepth: 10, Tenant: "test-tenant",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go func() { _ = eng.Run(ctx) }()

	// Wait for a published snapshot reflecting the folded delta.
	deadline := time.After(400 * time.Millisecond)
	for {
		if ev, ok := cap.lastOn(SubjectBookSnapshot); ok {
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
		SnapshotInterval: 10 * time.Millisecond, SnapshotDepth: 10, Tenant: "test-tenant",
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
		SnapshotInterval: time.Second, Tenant: "test-tenant",
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

// A SNAPSHOT MUST CARRY ITS OWN TENANT, not borrow one from whoever wired the
// producer.
//
// Snapshots publish off a ticker, outside any bus delivery, so bus.Consumer has
// stashed nothing on ctx. Before Config.Tenant existed the tenant came solely
// from the caller's ProducerConfig.Tenant — which meant this library was correct
// under market-ingest's wiring and would have had every snapshot rejected as
// "tenant_id required" under the multi-tenant arrangement pkg/alpha documents,
// where callers leave that fallback empty and supply tenants per event.
//
// Stamping it here makes the engine correct under ANY caller's wiring, which is
// the same property the venue adapters already have.
func TestEngine_SnapshotCarriesTheConfiguredTenant(t *testing.T) {
	cap := &capture{}
	src := &scriptedSource{updates: []depth.Update{
		{Snapshot: &marketpb.OrderBookSnapshot{
			InstrumentId: "BTC-USD", LastUpdateSequence: 1,
			Bids: []*marketpb.PriceLevel{lvl("50000", "1")},
			Asks: []*marketpb.PriceLevel{lvl("50001", "1")},
			// Not optional on the wire, so not optional here: since #957 the fold
			// REFUSES a snapshot with no venue time rather than stamping it 1970.
			EventTime: tsOf(time.Now().UTC()),
		}},
	}}
	eng := New(Config{
		Book: book.New("BTC-USD", "BTCUSDT", "BINANCE"), Source: src, Publisher: cap,
		SnapshotInterval: 10 * time.Millisecond, SnapshotDepth: 10, Tenant: "acme",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	go func() { _ = eng.Run(ctx) }()
	<-ctx.Done()

	got, ok := cap.last()
	if !ok {
		t.Fatal("no snapshot published")
	}
	if got.TenantID != "acme" {
		t.Fatalf("snapshot TenantID = %q, want %q — without it the tenant depends "+
			"entirely on the caller's ProducerConfig.Tenant, and a caller that sets "+
			"none has every snapshot rejected", got.TenantID, "acme")
	}
}

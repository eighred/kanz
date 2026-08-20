package ingest

// EXEC-M17 — the replay defence was a map in one process, and it consumed a nonce
// before the signal had actually gone anywhere.
//
// Two failures, and the second is the one nobody had named:
//
//  1. TWO PODS ARE TWO CACHES. A re-delivered alert landing on the other replica is
//     admitted a SECOND time and fans out a SECOND set of orders. The OMS's admission
//     gate cannot close it: a fresh claim mints a fresh order_id.
//  2. A FAILED SIGNAL BURNED ITS NONCE. Authenticate consumed the nonce, and then the
//     publish could still fail — so a transient broker error made the alert
//     PERMANENTLY unreplayable and the trading signal was lost, silently.

import (
	"context"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/signal/translate"
	"github.com/eighred/kanz/pkg/bus"
)

// brokenPublisher is a broker that is down — the ordinary transient failure.
type brokenPublisher struct {
	fail   bool
	events []bus.Event
}

func (b *brokenPublisher) Publish(_ context.Context, e bus.Event) error {
	if b.fail {
		return errors.New("nats: no responders")
	}
	b.events = append(b.events, e)
	return nil
}

// submitted returns the SubmitOrder commands published.
func (b *brokenPublisher) submitted() []*orderpb.SubmitOrder {
	var out []*orderpb.SubmitOrder
	for _, e := range b.events {
		if c, ok := e.Payload.(*orderpb.SubmitOrder); ok {
			out = append(out, c)
		}
	}
	return out
}

func (b *brokenPublisher) commands() int {
	n := 0
	for _, e := range b.events {
		if _, ok := e.Payload.(*orderpb.SubmitOrder); ok {
			n++
		}
	}
	return n
}

// pipelineOver builds a pipeline sharing the given nonce store and publisher — the
// nonce store is the thing that stands in for "one Redis, many pods".
func pipelineOver(t *testing.T, nonces NonceStore, pub translate.Publisher) *Pipeline {
	t.Helper()
	auth := NewAuthenticator(StaticSecrets{"momentum": testSecret}, nil, time.Minute, time.Now,
		WithNonceStore(nonces))
	p, err := NewPipeline(Options{
		Auth:      auth,
		Symbols:   StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
		Prices:    StaticPrices{"BTC-USD": big.NewRat(50000, 1)},
		Equity:    StaticEquity{"fund-alpha": big.NewRat(1_000_000, 1)},
		Positions: StaticPositions{},
		Alloc:     StaticAllocation{"fund-alpha": {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}}},
		Publisher: pub,
		Gate:      halt.OpenGate(nil),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// pipelineWithPositions builds a two-venue pipeline over a REAL position source — the
// binding production was missing (EXEC-M19b).
func pipelineWithPositions(t *testing.T, positions translate.PositionSource, pub translate.Publisher) *Pipeline {
	t.Helper()
	auth := NewAuthenticator(StaticSecrets{"momentum": testSecret}, nil, time.Minute, time.Now,
		WithNonceStore(NewMemoryNonces(time.Minute, 1000)))
	p, err := NewPipeline(Options{
		Auth:      auth,
		Symbols:   StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
		Prices:    StaticPrices{"BTC-USD": big.NewRat(50000, 1)},
		Equity:    StaticEquity{"fund-alpha": big.NewRat(1_000_000, 1)},
		Positions: positions,
		Alloc: StaticAllocation{"fund-alpha": {
			{Venue: "BINANCE", Weight: big.NewRat(6, 10)},
			{Venue: "OKX", Weight: big.NewRat(4, 10)},
		}},
		Publisher: pub,
		Gate:      halt.OpenGate(nil),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

func post(t *testing.T, p *Pipeline, raw string) error {
	t.Helper()
	_, err := p.Process(context.Background(), []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret))
	return err
}

// TestOneNonceCacheAcrossPods: the pods share ONE nonce store, so the alert that pod A
// admitted is a REPLAY at pod B.
//
// This is the whole reason webhook-ingest is pinned to a single replica: without a
// shared store, pod B admits the re-delivered alert and fans out a SECOND set of
// orders against a live exchange, and nothing downstream can catch it — the second
// signal carries a fresh signal_id, so it mints fresh order_ids and JetStream's
// dedup sees two different orders.
func TestOneNonceCacheAcrossPods(t *testing.T) {
	nonces := NewMemoryNonces(time.Minute, 1000) // the shared store; RedisNonces in production
	pubA, pubB := &brokenPublisher{}, &brokenPublisher{}
	podA := pipelineOver(t, nonces, pubA)
	podB := pipelineOver(t, nonces, pubB)

	raw := body("buy", "1", "absolute_qty", "alert-1")

	if err := post(t, podA, raw); err != nil {
		t.Fatalf("pod A rejected a good alert: %v", err)
	}
	if err := post(t, podB, raw); !errors.Is(err, ErrReplayed) {
		t.Fatalf("pod B admitted an alert pod A had already taken: err = %v, want ErrReplayed", err)
	}
	if n := pubB.commands(); n != 0 {
		t.Fatalf("pod B fanned out %d orders for an alert that was already traded — a DOUBLE TRADE", n)
	}
}

// TestAFailedSignalReleasesItsNonce: the broker was down, so nothing was published and
// nobody traded. The alert must be re-deliverable — burning the nonce here loses the
// trading signal forever, which is the loss EXEC-M7b's three-phase lease exists to
// prevent ("claim-and-never-release turns a failed handler into a lost event").
//
// The retry is safe by construction: signal_id is DeterministicID(strategy, nonce), so
// a redelivery re-derives the same idempotency keys and the broker collapses anything
// that did land.
func TestAFailedSignalReleasesItsNonce(t *testing.T) {
	nonces := NewMemoryNonces(time.Minute, 1000)
	pub := &brokenPublisher{fail: true}
	p := pipelineOver(t, nonces, pub)

	raw := body("buy", "1", "absolute_qty", "alert-2")

	if err := post(t, p, raw); err == nil {
		t.Fatal("Process succeeded against a broker that is down")
	}

	// The broker comes back. TradingView re-delivers the SAME alert.
	pub.fail = false
	if err := post(t, p, raw); err != nil {
		t.Fatalf("the re-delivered alert was refused after the broker recovered: %v — the signal is LOST", err)
	}
	if n := pub.commands(); n == 0 {
		t.Fatal("the recovered retry produced no orders")
	}
}

// TestASuccessfulSignalHoldsItsNonce: the ordinary case must not regress. Once the
// alert has actually been acted on, a redelivery is a replay.
func TestASuccessfulSignalHoldsItsNonce(t *testing.T) {
	nonces := NewMemoryNonces(time.Minute, 1000)
	pub := &brokenPublisher{}
	p := pipelineOver(t, nonces, pub)

	raw := body("buy", "1", "absolute_qty", "alert-3")

	if err := post(t, p, raw); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if err := post(t, p, raw); !errors.Is(err, ErrReplayed) {
		t.Fatalf("a re-delivered alert that ALREADY TRADED = %v, want ErrReplayed", err)
	}
	if n := pub.commands(); n != 1 {
		t.Fatalf("orders fanned out = %d, want 1 — the replay traded again", n)
	}
}

// Package depth defines the exchange-depth transport seam for market-ingest and
// a deterministic in-process simulator that satisfies it. A DepthSource yields
// the raw L2 feed — an initial snapshot followed by incremental deltas — which
// the ingester folds into the in-memory book. Real venues (Binance/OKX depth
// websockets) implement DepthSource behind a per-venue build tag, isolated from
// the default binary exactly like the OMS exchange connectors; the sim is the
// vendor-free default so the fold + snapshot path is exercised without a network
// and without fabricating live market data (the sim is explicitly labelled SIM).
package depth

import (
	"context"
	"math/big"
	"time"

	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/services/market-ingest/internal/dec"
)

// Update is one message from a depth feed: exactly one of Snapshot or Delta is
// set. A Snapshot resets the book (bootstrap or gap recovery); a Delta folds
// incrementally.
type Update struct {
	Snapshot *marketpb.OrderBookSnapshot
	Delta    *marketpb.OrderBookDelta
}

// DepthSource is the depth-feed transport seam. Recv blocks until the next
// update or ctx cancellation. Implementations own reconnect/resequencing; on an
// unrecoverable stream error they return it and the ingester rebinds.
type DepthSource interface {
	Recv(ctx context.Context) (Update, error)
}

// SimSource is a deterministic synthetic depth generator — the default,
// vendor-free DepthSource. It emits one snapshot then a bounded, seeded stream
// of deltas that random-walk the mid and toggle a level, so the fold + snapshot
// pipeline is certifiable end to end. It is a SIMULATION, never presented as a
// real venue: mic is "SIM".
type SimSource struct {
	instrumentID string
	symbol       string
	interval     time.Duration
	mid          *big.Rat
	seq          uint64
	rng          uint64 // xorshift state (deterministic)
	emittedSnap  bool
}

// SimConfig configures a SimSource.
type SimConfig struct {
	InstrumentID string
	Symbol       string
	Interval     time.Duration // delay between updates (<=0 ⇒ 10ms)
	Mid          *big.Rat      // starting mid price (nil ⇒ 50000)
	Seed         uint64        // deterministic seed (0 ⇒ 1)
}

// NewSimSource builds a deterministic simulator.
func NewSimSource(cfg SimConfig) *SimSource {
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Millisecond
	}
	mid := cfg.Mid
	if mid == nil {
		mid = big.NewRat(50000, 1)
	}
	seed := cfg.Seed
	if seed == 0 {
		seed = 1
	}
	return &SimSource{
		instrumentID: cfg.InstrumentID, symbol: cfg.Symbol,
		interval: cfg.Interval, mid: new(big.Rat).Set(mid), rng: seed,
	}
}

func (s *SimSource) next() uint64 {
	// xorshift64 — deterministic, dependency-free.
	s.rng ^= s.rng << 13
	s.rng ^= s.rng >> 7
	s.rng ^= s.rng << 17
	return s.rng
}

// Recv emits the initial snapshot, then one delta per interval.
func (s *SimSource) Recv(ctx context.Context) (Update, error) {
	select {
	case <-ctx.Done():
		return Update{}, ctx.Err()
	case <-time.After(s.interval):
	}

	if !s.emittedSnap {
		s.emittedSnap = true
		s.seq++
		return Update{Snapshot: &marketpb.OrderBookSnapshot{
			InstrumentId:       s.instrumentID,
			Symbol:             s.symbol,
			Mic:                "SIM",
			EventTime:          timestamppb.New(time.Now().UTC()),
			LastUpdateSequence: s.seq,
			Bids:               []*marketpb.PriceLevel{s.level(s.bidPx(1), 2), s.level(s.bidPx(2), 3)},
			Asks:               []*marketpb.PriceLevel{s.level(s.askPx(1), 2), s.level(s.askPx(2), 3)},
		}}, nil
	}

	// Random-walk the mid by ±1 and refresh the near touch on one side.
	if s.next()%2 == 0 {
		s.mid.Add(s.mid, big.NewRat(1, 1))
	} else {
		s.mid.Sub(s.mid, big.NewRat(1, 1))
	}
	prev := s.seq
	s.seq++
	side := s.bidPx(1)
	bids, asks := []*marketpb.PriceLevel{s.level(side, int64(1+s.next()%5))}, []*marketpb.PriceLevel(nil)
	if s.next()%2 == 0 {
		bids, asks = nil, []*marketpb.PriceLevel{s.level(s.askPx(1), int64(1+s.next()%5))}
	}
	return Update{Delta: &marketpb.OrderBookDelta{
		InstrumentId:        s.instrumentID,
		Symbol:              s.symbol,
		Mic:                 "SIM",
		EventTime:           timestamppb.New(time.Now().UTC()),
		FirstUpdateSequence: s.seq,
		LastUpdateSequence:  s.seq,
		PrevUpdateSequence:  prev,
		Bids:                bids,
		Asks:                asks,
	}}, nil
}

func (s *SimSource) bidPx(n int64) *big.Rat { return new(big.Rat).Sub(s.mid, big.NewRat(n, 1)) }
func (s *SimSource) askPx(n int64) *big.Rat { return new(big.Rat).Add(s.mid, big.NewRat(n, 1)) }

func (s *SimSource) level(price *big.Rat, size int64) *marketpb.PriceLevel {
	return &marketpb.PriceLevel{Price: dec.ToProto(price), Size: dec.ToProto(big.NewRat(size, 1))}
}

var _ DepthSource = (*SimSource)(nil)

package trades

import (
	"context"
	"math/big"
	"time"
)

// SimSource is a deterministic synthetic trade generator — the default,
// vendor-free TradeSource. It alternates aggressor sides on a seeded walk so the
// tape + volume aggregation are certifiable without a network. It is a
// SIMULATION and is never presented as real market data.
type SimSource struct {
	interval time.Duration
	price    *big.Rat
	rng      uint64
}

// SimConfig configures a SimSource.
type SimConfig struct {
	Interval time.Duration // delay between trades (<=0 ⇒ 10ms)
	Price    *big.Rat      // starting price (nil ⇒ 50000)
	Seed     uint64        // deterministic seed (0 ⇒ 1)
}

// NewSimSource builds a deterministic simulator.
func NewSimSource(cfg SimConfig) *SimSource {
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Millisecond
	}
	price := cfg.Price
	if price == nil {
		price = big.NewRat(50000, 1)
	}
	seed := cfg.Seed
	if seed == 0 {
		seed = 1
	}
	return &SimSource{interval: cfg.Interval, price: new(big.Rat).Set(price), rng: seed}
}

func (s *SimSource) next() uint64 {
	// xorshift64 — deterministic, dependency-free.
	s.rng ^= s.rng << 13
	s.rng ^= s.rng >> 7
	s.rng ^= s.rng << 17
	return s.rng
}

// Recv emits one synthetic trade per interval.
func (s *SimSource) Recv(ctx context.Context) (Trade, error) {
	select {
	case <-ctx.Done():
		return Trade{}, ctx.Err()
	case <-time.After(s.interval):
	}
	side := SideBuy
	if s.next()%2 == 0 {
		side = SideSell
		s.price.Sub(s.price, big.NewRat(1, 1))
	} else {
		s.price.Add(s.price, big.NewRat(1, 1))
	}
	return Trade{
		Price:     new(big.Rat).Set(s.price),
		Size:      big.NewRat(int64(1+s.next()%5), 100),
		TakerSide: side,
		EventTime: time.Now().UTC(),
	}, nil
}

var _ TradeSource = (*SimSource)(nil)

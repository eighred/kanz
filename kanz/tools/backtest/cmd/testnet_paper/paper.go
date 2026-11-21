package main

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/tools/backtest"
	"github.com/eighred/kanz/tools/replay"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type crossover struct{ closes []*big.Rat }

func (s *crossover) OnEvent(_ context.Context, ev backtest.Event, pit backtest.PointInTime) ([]backtest.Decision, error) {
	var closePrice commonpb.Decimal
	if err := proto.Unmarshal(ev.Payload, &closePrice); err != nil {
		return nil, err
	}
	s.closes = append(s.closes, dec.FromProto(&closePrice))
	if len(s.closes) < warmupBars {
		return nil, nil
	}
	s.closes = s.closes[len(s.closes)-warmupBars:]
	total, recent := new(big.Rat), new(big.Rat)
	for i, price := range s.closes {
		total.Add(total, price)
		if i >= warmupBars-5 {
			recent.Add(recent, price)
		}
	}
	// Compare exact sums at the common denominator, including equality.
	action := "cash"
	if recent.Mul(recent, big.NewRat(4, 1)).Cmp(total) > 0 {
		action = "long"
	}
	return []backtest.Decision{{EventID: ev.Envelope.GetEventId(), InstrumentID: "BTCUSDT", AsOf: pit.AsOf(), Action: action}}, nil
}

type fill struct {
	SignalTime time.Time `json:"signal_close_utc"`
	FillTime   time.Time `json:"fill_open_utc"`
	Side       string    `json:"side"`
	Quantity   string    `json:"btc"`
	Open       string    `json:"open_usdt"`
	Price      string    `json:"fill_usdt"`
	Fee        string    `json:"fee_usdt"`
}

type report struct {
	Experiment  string `json:"experiment"`
	Day         string `json:"utc_day"`
	Hash        string `json:"input_sha256"`
	WarmupBars  int    `json:"warmup_bars"`
	DayBars     int    `json:"evaluated_bars"`
	Initial     string `json:"initial_cash_usdt"`
	FeeBPS      int64  `json:"assumed_fee_bps_per_fill"`
	SlippageBPS int64  `json:"assumed_adverse_slippage_bps_per_fill"`
	Cash        string `json:"closing_cash_usdt"`
	Quantity    string `json:"closing_btc"`
	FinalPrice  string `json:"last_close_usdt"`
	Marked      string `json:"marked_equity_usdt"`
	Fees        string `json:"fill_fees_usdt"`
	Slippage    string `json:"fill_slippage_usdt"`
	ExitCost    string `json:"hypothetical_exit_cost_usdt"`
	Liquidation string `json:"net_liquidation_value_usdt"`
	PnL         string `json:"net_pnl_usdt"`
	Return      string `json:"net_return_pct"`
	Drawdown    string `json:"max_close_to_close_drawdown_pct"`
	Fills       []fill `json:"fills"`
}

const feeBPS, slippageBPS int64 = 10, 5

var (
	initial      = big.NewRat(1000, 1)
	feeRate      = big.NewRat(feeBPS, 10000)
	slippageRate = big.NewRat(slippageBPS, 10000)
)

func purchaseQuantity(cash, price *big.Rat) *big.Rat {
	unitCost := new(big.Rat).Mul(price, new(big.Rat).Add(big.NewRat(1, 1), feeRate))
	units := new(big.Rat).Quo(cash, unitCost)
	units.Mul(units, big.NewRat(100000000, 1))
	atoms := new(big.Int).Quo(units.Num(), units.Denom())
	return new(big.Rat).SetFrac(atoms, big.NewInt(100000000))
}

func simulate(ctx context.Context, bars []bar) (report, error) {
	if len(bars) <= warmupBars {
		return report{}, fmt.Errorf("no evaluation bars")
	}
	events := make([]replay.Event, len(bars))
	for i, b := range bars {
		when := b.start.Add(interval - time.Millisecond)
		payload, err := proto.Marshal(b.close)
		if err != nil {
			return report{}, err
		}
		events[i] = replay.Event{Envelope: &envelopepb.Envelope{EventId: when.Format(time.RFC3339Nano), EventTime: timestamppb.New(when)}, Payload: payload, Offset: int64(i)}
	}
	replayed, err := (&backtest.Harness{}).Run(ctx, backtest.NewSliceSource(events), &crossover{})
	if err != nil {
		return report{}, err
	}
	cash, btc := new(big.Rat).Set(initial), new(big.Rat)
	fees, slippage := new(big.Rat), new(big.Rat)
	peak, worst := new(big.Rat).Set(initial), new(big.Rat)
	r := report{Experiment: "BTCUSDT-5m-SMA5-SMA20-paper-v1", Day: bars[warmupBars].start.UTC().Format(time.DateOnly),
		WarmupBars: warmupBars, DayBars: len(bars) - warmupBars, Initial: initial.FloatString(2), FeeBPS: feeBPS, SlippageBPS: slippageBPS, Fills: []fill{}}
	for i, b := range bars[warmupBars:] {
		decision := replayed.Decisions[i] // decision from the preceding completed bar
		if !decision.AsOf.Equal(b.start.Add(-time.Millisecond)) {
			return report{}, fmt.Errorf("signal and fill times do not align")
		}
		long := decision.Action == "long"
		if long != (btc.Sign() > 0) {
			side := "sell"
			delta := new(big.Rat).Mul(b.open, slippageRate)
			price := new(big.Rat).Sub(b.open, delta)
			quantity := new(big.Rat).Set(btc)
			if long {
				side = "buy"
				price.Add(b.open, delta)
				quantity = purchaseQuantity(cash, price)
				if quantity.Sign() == 0 {
					return report{}, fmt.Errorf("cash cannot fund one simulated quantity step")
				}
			}
			notional := new(big.Rat).Mul(quantity, price)
			fee := new(big.Rat).Mul(notional, feeRate)
			if long {
				cash.Sub(cash, notional)
				btc.Set(quantity)
			} else {
				cash.Add(cash, notional)
				btc.SetInt64(0)
			}
			cash.Sub(cash, fee)
			fees.Add(fees, fee)
			slippage.Add(slippage, new(big.Rat).Mul(quantity, delta))
			r.Fills = append(r.Fills, fill{decision.AsOf, b.start, side, quantity.FloatString(8), b.open.FloatString(8), price.FloatString(12), fee.FloatString(12)})
		}
		mark := new(big.Rat).Add(cash, new(big.Rat).Mul(btc, dec.FromProto(b.close)))
		if mark.Cmp(peak) > 0 {
			peak.Set(mark)
		}
		drop := new(big.Rat).Quo(new(big.Rat).Sub(peak, mark), peak)
		if drop.Cmp(worst) > 0 {
			worst.Set(drop)
		}
	}
	finalPrice := dec.FromProto(bars[len(bars)-1].close)
	holding := new(big.Rat).Mul(btc, finalPrice)
	marked := new(big.Rat).Add(cash, holding)
	exitSlip := new(big.Rat).Mul(holding, slippageRate)
	exitFee := new(big.Rat).Mul(new(big.Rat).Sub(holding, exitSlip), feeRate)
	exitCost := new(big.Rat).Add(exitSlip, exitFee)
	liquidation := new(big.Rat).Sub(marked, exitCost)
	pnl := new(big.Rat).Sub(liquidation, initial)
	pct := new(big.Rat).Mul(new(big.Rat).Quo(pnl, initial), big.NewRat(100, 1))
	r.Cash, r.Quantity, r.FinalPrice = cash.FloatString(8), btc.FloatString(8), finalPrice.FloatString(8)
	r.Marked, r.Fees, r.Slippage = marked.FloatString(8), fees.FloatString(8), slippage.FloatString(8)
	r.ExitCost, r.Liquidation, r.PnL = exitCost.FloatString(8), liquidation.FloatString(8), pnl.FloatString(8)
	r.Return, r.Drawdown = pct.FloatString(8), worst.Mul(worst, big.NewRat(100, 1)).FloatString(8)
	return r, nil
}

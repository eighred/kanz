package bridge

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/optimization"
	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ExactExecutionBounds are immutable approval inputs. Missing instrument terms
// refuse: rounding a quantity here would change the book the mandate evaluated.
type ExactExecutionBounds struct {
	MaxNotional    dec.Exact
	MaxBuyNotional dec.Exact
	SlippageBPS    uint32
	ExpiresAt      time.Time
	Lots           map[string]dec.Exact
	Ticks          map[string]dec.Exact
}

var ErrExactExecution = errors.New("rebalance execution terms are invalid or unrepresentable")

// ExactOrders constructs commands, not an authorization. The lifecycle must
// load the immutable revision, check approval and current inputs, reserve cash,
// and enqueue these commands atomically. Ordinary OMS controls still apply.
// No ParentOrderId is set: these are independent trades, not admitted slices.
func ExactOrders(p optimization.ExactRebalanceProposal, tenant, revision, issuer string, bounds ExactExecutionBounds, freshness Freshness) ([]*orderpb.SubmitOrder, error) {
	for _, id := range []string{tenant, revision, issuer, p.PortfolioID} {
		if strings.TrimSpace(id) == "" || len(id) > 256 {
			return nil, ErrExactExecution
		}
	}
	if len(p.Trades) == 0 || len(p.Trades) > optimization.MaxProposalInstruments {
		return nil, ErrExactExecution
	}
	if _, err := freshness.Check(optimization.RebalanceProposal{AsOf: p.AsOf}); err != nil {
		return nil, err
	}
	if p.MandateStatus != optimization.MandateFeasible {
		if p.MandateStatus == optimization.MandateInfeasible {
			return nil, ErrMandateInfeasible
		}
		return nil, ErrMandateUnchecked
	}
	expires := timestamppb.New(bounds.ExpiresAt.UTC())
	if expires.CheckValid() != nil || !bounds.ExpiresAt.After(freshness.now()) || !bounds.ExpiresAt.After(p.AsOf) || bounds.SlippageBPS >= 10000 {
		return nil, ErrExactExecution
	}
	maxGross, err := bounds.MaxNotional.Rat()
	if err != nil || maxGross.Sign() <= 0 {
		return nil, ErrNotionalUnbounded
	}
	maxBuy, err := bounds.MaxBuyNotional.Rat()
	if err != nil || maxBuy.Sign() < 0 {
		return nil, ErrExactExecution
	}
	trades := append([]optimization.ExactTrade(nil), p.Trades...)
	sort.Slice(trades, func(i, j int) bool { return trades[i].InstrumentID < trades[j].InstrumentID })
	out := make([]*orderpb.SubmitOrder, 0, len(trades))
	gross, buys := new(big.Rat), new(big.Rat)
	for i, tr := range trades {
		if strings.TrimSpace(tr.InstrumentID) == "" || len(tr.InstrumentID) > 256 || (i > 0 && trades[i-1].InstrumentID == tr.InstrumentID) || (tr.Side != optimization.Buy && tr.Side != optimization.Sell) {
			return nil, ErrExactExecution
		}
		quantity, err := positiveExact(tr.Quantity)
		if err != nil {
			return nil, err
		}
		notional, err := positiveExact(tr.Notional)
		if err != nil {
			return nil, err
		}
		lot, err := positiveExact(bounds.Lots[tr.InstrumentID])
		if err != nil {
			return nil, err
		}
		tick, err := positiveExact(bounds.Ticks[tr.InstrumentID])
		if err != nil {
			return nil, err
		}
		if !new(big.Rat).Quo(quantity, lot).IsInt() {
			return nil, fmt.Errorf("%w: quantity is not a whole lot for %s", ErrExactExecution, tr.InstrumentID)
		}
		q, ok := dec.ToProtoExact(quantity)
		if !ok {
			return nil, ErrExactExecution
		}
		price := new(big.Rat).Quo(notional, quantity)
		adjustment := int64(10000 + bounds.SlippageBPS)
		if tr.Side == optimization.Sell {
			adjustment = int64(10000 - bounds.SlippageBPS)
		}
		price.Mul(price, big.NewRat(adjustment, 10000))
		// Tick alignment only tightens the approved limit: floor buys, ceil
		// sells. Zero slippage means the reference price, never an unbounded MARKET.
		units := new(big.Rat).Quo(price, tick)
		whole, remainder := new(big.Int), new(big.Int)
		whole.QuoRem(units.Num(), units.Denom(), remainder)
		if tr.Side == optimization.Sell && remainder.Sign() != 0 {
			whole.Add(whole, big.NewInt(1))
		}
		price.Mul(new(big.Rat).SetInt(whole), tick)
		limit, ok := dec.ToProtoExact(price)
		if !ok || price.Sign() <= 0 {
			return nil, ErrExactExecution
		}
		// Bound buys at their worst executable price. Reference notional alone
		// would let the approved slippage allowance exceed the capital ceiling.
		boundedNotional := new(big.Rat).Set(notional)
		if tr.Side == optimization.Buy {
			worst := new(big.Rat).Mul(quantity, price)
			buys.Add(buys, worst)
			if worst.Cmp(boundedNotional) > 0 {
				boundedNotional.Set(worst)
			}
		}
		gross.Add(gross, boundedNotional)
		if gross.Cmp(maxGross) > 0 {
			return nil, ErrNotionalExceeded
		}
		if buys.Cmp(maxBuy) > 0 {
			return nil, fmt.Errorf("%w: worst-price buys exceed cash bound", ErrExactExecution)
		}
		id := "rebal:" + dualcontrol.Digest("rebalance-child-v1", tenant, p.PortfolioID, revision, tr.InstrumentID)
		out = append(out, &orderpb.SubmitOrder{
			Metadata: &commandpb.CommandMetadata{Issuer: issuer, TargetId: id, Reason: "approved rebalance revision " + revision},
			OrderId:  id, PortfolioId: p.PortfolioID, InstrumentId: tr.InstrumentID,
			Side: orderSide(tr.Side), Quantity: q, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
			LimitPrice: limit, TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_GTD,
			ExpireAt: timestamppb.New(bounds.ExpiresAt.UTC()),
		})
	}
	return out, nil
}

func positiveExact(value dec.Exact) (*big.Rat, error) {
	r, err := value.Rat()
	if err != nil || r.Sign() <= 0 {
		return nil, ErrExactExecution
	}
	return r, nil
}

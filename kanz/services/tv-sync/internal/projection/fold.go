package projection

import (
	"math/big"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/costbasis"
)

// posState is the folded state of one instrument's position: signed net
// quantity, the average entry price of the open position, and cumulative
// realized P&L (NET OF FEES here — see applyExecution).
//
// An alias to the platform's lot (#428). This package used to carry its own
// struct and its own copy of the weighted-average-cost arithmetic, one of three
// such copies; they had already drifted from each other.
type posState = costbasis.Lot

// foldPositions replays executions (in fold order) into per-instrument positions
// using the average-cost method, exactly in big.Rat. This is a pure fold — the
// same executions always produce the same positions (determinism).
func foldPositions(execs []execution) map[string]*posState {
	return foldPositionsFrom(nil, execs)
}

// foldPositionsFrom folds execs onto a COPY of an existing fold.
//
// THE BASE IS WHAT RETENTION ALREADY ATE (#809). Once the resident history is a
// window rather than the whole of it, foldPositions(execs) is no longer the
// account's book — it is the book since the window opened, which reports a fund
// flat in an instrument it has held for a year. The evicted prefix is folded
// into account.baseline as it leaves, and every fold that claims to be the
// account's position book starts from it.
//
// IT COPIES RATHER THAN FOLDING IN PLACE. applyExecution mutates a *posState, so
// folding straight onto the caller's base would advance the baseline itself by
// every as-of read — a read silently rewriting the fund's realized P&L, which is
// the one class of defect this projection's package comment promises it cannot
// have.
func foldPositionsFrom(base map[string]*posState, execs []execution) map[string]*posState {
	out := cloneLots(base)
	for _, e := range execs {
		p := out[e.instrument]
		if p == nil {
			p = costbasis.NewLot()
			out[e.instrument] = p
		}
		applyExecution(p, e)
	}
	return out
}

// cloneLots deep-copies a fold. The rationals are copied too: a shallow copy
// would share the *big.Rat behind every field, which is the in-place mutation
// foldPositionsFrom exists to avoid.
func cloneLots(in map[string]*posState) map[string]*posState {
	out := make(map[string]*posState, len(in))
	for inst, p := range in {
		out[inst] = &costbasis.Lot{
			Qty:      new(big.Rat).Set(p.Qty),
			AvgCost:  new(big.Rat).Set(p.AvgCost),
			Realized: new(big.Rat).Set(p.Realized),
		}
	}
	return out
}

// applyExecution folds one execution, netting its fee out of realized P&L.
//
// THE FEE LINE IS THIS SURFACE'S POLICY, AND IT IS DELIBERATELY VISIBLE HERE
// (#428). costbasis.Fold computes GROSS realized P&L, because that is the
// arithmetic every consumer shares; whether fees reduce a reported realized
// figure is a reporting decision that differs by surface.
//
// It differed silently before. This projection subtracted fees while the OMS
// position book and the accounting IBOR did not, so "what has this position
// realized" had two answers depending on which screen a person was looking at —
// and nothing said so, because each fold was a separate copy of the calculation
// with the fee line buried inside one of them. Moving the shared arithmetic out
// left this line where a reader can see that TradingView is shown a net figure.
func applyExecution(p *posState, e execution) {
	if e.fee != nil {
		p.Realized.Sub(p.Realized, e.fee)
	}
	signed := new(big.Rat).Set(e.qty)
	if e.side == orderpb.Side_SIDE_SELL {
		signed.Neg(signed)
	}
	costbasis.Fold(p, signed, e.price)
}

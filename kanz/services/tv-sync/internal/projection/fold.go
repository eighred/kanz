package projection

import (
	"math/big"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// posState is the folded state of one instrument's position: signed net
// quantity, the average entry price of the open position, and cumulative
// realized P&L (net of fees).
type posState struct {
	net      *big.Rat // signed: >0 long, <0 short
	avg      *big.Rat // average entry price of the currently-open position
	realized *big.Rat // cumulative realized P&L, net of fees
}

// foldPositions replays executions (in fold order) into per-instrument positions
// using the average-cost method, exactly in big.Rat. Increasing a position
// updates the weighted-average entry; reducing it realizes P&L against that
// average; crossing through zero flips the position and re-bases the average at
// the crossing price. Fees reduce realized P&L. This is a pure fold — the same
// executions always produce the same positions (determinism).
func foldPositions(execs []execution) map[string]*posState {
	out := make(map[string]*posState)
	for _, e := range execs {
		p := out[e.instrument]
		if p == nil {
			p = &posState{net: new(big.Rat), avg: new(big.Rat), realized: new(big.Rat)}
			out[e.instrument] = p
		}
		applyExecution(p, e)
	}
	return out
}

func applyExecution(p *posState, e execution) {
	// Fees always reduce realized P&L.
	if e.fee != nil {
		p.realized.Sub(p.realized, e.fee)
	}
	signed := new(big.Rat).Set(e.qty)
	if e.side == orderpb.Side_SIDE_SELL {
		signed.Neg(signed)
	}

	// Opening or increasing (same direction, or flat): weighted-average entry.
	if p.net.Sign() == 0 || sameSign(p.net, signed) {
		absNet := new(big.Rat).Abs(p.net)
		absAdd := new(big.Rat).Abs(signed)
		newAbs := new(big.Rat).Add(absNet, absAdd)
		if newAbs.Sign() != 0 {
			// avg = (avg*|net| + price*|add|) / (|net| + |add|)
			num := new(big.Rat).Add(new(big.Rat).Mul(p.avg, absNet), new(big.Rat).Mul(e.price, absAdd))
			p.avg = new(big.Rat).Quo(num, newAbs)
		}
		p.net.Add(p.net, signed)
		return
	}

	// Reducing / closing (opposite direction): realize against the average.
	closeQty := new(big.Rat).Abs(signed)
	if absNet := new(big.Rat).Abs(p.net); closeQty.Cmp(absNet) > 0 {
		closeQty = absNet
	}
	// realized += (price - avg) * closeQty * sign(net)
	pnl := new(big.Rat).Mul(new(big.Rat).Sub(e.price, p.avg), closeQty)
	if p.net.Sign() < 0 {
		pnl.Neg(pnl)
	}
	p.realized.Add(p.realized, pnl)

	prevSign := p.net.Sign()
	p.net.Add(p.net, signed)
	switch {
	case p.net.Sign() == 0:
		p.avg = new(big.Rat) // flat
	case p.net.Sign() != prevSign:
		p.avg = new(big.Rat).Set(e.price) // flipped: new position opens at this price
	}
}

func sameSign(a, b *big.Rat) bool {
	return a.Sign() > 0 && b.Sign() > 0 || a.Sign() < 0 && b.Sign() < 0
}

// unrealized returns net × (mark − avg) for an open position, or nil when there
// is no mark. Long and short are handled by the sign of net.
func (p *posState) unrealized(mark *big.Rat) *big.Rat {
	if mark == nil || p.net.Sign() == 0 {
		return nil
	}
	return new(big.Rat).Mul(new(big.Rat).Sub(mark, p.avg), p.net)
}

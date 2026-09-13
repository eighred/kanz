package optimization

import (
	"errors"
	"math/big"
	"sort"
	"time"

	"github.com/eighred/kanz/internal/dec"
)

type ExactTrade struct {
	InstrumentID  string
	Side          TradeSide
	CurrentWeight dec.Exact
	TargetWeight  dec.Exact
	Quantity      dec.Exact
	Notional      dec.Exact
}

// ExactRebalanceProposal is an unapproved recommendation. Execution admission
// and mandate verification remain separate controls.
type ExactRebalanceProposal struct {
	PortfolioID   string
	Targets       map[string]dec.Exact
	Trades        []ExactTrade
	Turnover      dec.Exact
	AsOf          time.Time
	MandateStatus MandateStatus
}

func RebalanceExact(portfolioID string, current, target map[string]dec.Exact, nav dec.Exact, prices map[string]dec.Exact, threshold dec.Exact, asOf time.Time) (ExactRebalanceProposal, error) {
	fail := errors.New("optimization: exact rebalance inputs unavailable")
	value, err := nav.Rat()
	if err != nil || value.Sign() <= 0 {
		return ExactRebalanceProposal{}, fail
	}
	band, err := threshold.Rat()
	if err != nil || band.Sign() < 0 || band.Cmp(big.NewRat(1, 1)) > 0 || asOf.IsZero() {
		return ExactRebalanceProposal{}, fail
	}
	ids := map[string]bool{}
	for id := range current {
		ids[id] = true
	}
	for id := range target {
		ids[id] = true
	}
	if len(ids) > 4096 {
		return ExactRebalanceProposal{}, fail
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	out := ExactRebalanceProposal{PortfolioID: portfolioID, Targets: map[string]dec.Exact{}, AsOf: asOf}
	for id, v := range target {
		out.Targets[id] = v
	}
	gross := new(big.Rat)
	for _, id := range ordered {
		cw, tw := dec.Exact("0"), dec.Exact("0")
		if v, ok := current[id]; ok {
			cw = v
		}
		if v, ok := target[id]; ok {
			tw = v
		}
		c, err := cw.Rat()
		if err != nil {
			return ExactRebalanceProposal{}, fail
		}
		t, err := tw.Rat()
		if err != nil {
			return ExactRebalanceProposal{}, fail
		}
		delta := new(big.Rat).Sub(t, c)
		magnitude := new(big.Rat).Abs(delta)
		if magnitude.Cmp(band) <= 0 {
			continue
		}
		price, err := prices[id].Rat()
		if err != nil || price.Sign() <= 0 {
			return ExactRebalanceProposal{}, fail
		}
		notional := new(big.Rat).Mul(magnitude, value)
		n, err := dec.ExactFromRat(notional)
		if err != nil {
			return ExactRebalanceProposal{}, err
		}
		q, err := dec.ExactFromRat(new(big.Rat).Quo(notional, price))
		if err != nil {
			return ExactRebalanceProposal{}, err
		}
		side := Buy
		if delta.Sign() < 0 {
			side = Sell
		}
		out.Trades = append(out.Trades, ExactTrade{InstrumentID: id, Side: side, CurrentWeight: cw, TargetWeight: tw, Notional: n, Quantity: q})
		gross.Add(gross, magnitude)
	}
	out.Turnover, err = dec.ExactFromRat(new(big.Rat).Quo(gross, big.NewRat(2, 1)))
	return out, err
}

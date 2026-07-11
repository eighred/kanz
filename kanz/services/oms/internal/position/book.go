// Package position is the OMS's fill→position projector (OMS-01e). It folds
// order Fill FACTs into a per-(portfolio,instrument) holding using exact
// weighted-average-cost accounting, and republishes the result as a
// domain.v1.PositionState on risk.position.changed — the SAME FACT the
// risk-engine already ingests (ingest.EventTypePositionChanged), so traded
// positions flow into exposures/measures through the existing path with no
// change to the risk engine.
//
// Currency: a fill carries no currency, so the projector stamps Money in a
// configured base currency. The real per-instrument currency comes from a
// reference-data join (MODEL-01 carried-forward); until then BaseCurrency is
// the portfolio reporting currency.
package position

import (
	"math/big"
	"sync"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/internal/dec"
)

type key struct{ portfolio, instrument string }

// lot is the running state of one holding: signed quantity, the average cost of
// the open position (always non-negative), and cumulative realized P&L.
type lot struct {
	qty      *big.Rat // signed: + long, - short
	avg      *big.Rat // average cost per unit of the open lot
	realized *big.Rat
}

// Book accumulates holdings from fills. Goroutine-safe.
type Book struct {
	mu      sync.Mutex
	lots    map[key]*lot
	baseCcy string
}

// NewBook returns an empty book stamping Money in baseCcy.
func NewBook(baseCcy string) *Book {
	if baseCcy == "" {
		baseCcy = "USD"
	}
	return &Book{lots: make(map[key]*lot), baseCcy: baseCcy}
}

// Apply folds one fill into the book and returns the resulting PositionState.
// Realized P&L is booked on the portion of a fill that reduces or closes an
// opposite position; a fill that crosses through zero opens a new lot at the
// fill price. market_value and unrealized_pnl are marked at the fill price (the
// latest trade), until a market-data mark is wired.
func (b *Book) Apply(portfolioID string, fill *orderpb.Fill, asOf time.Time) *domainpb.PositionState {
	b.mu.Lock()
	defer b.mu.Unlock()

	k := key{portfolioID, fill.GetInstrumentId()}
	l := b.lots[k]
	if l == nil {
		l = &lot{qty: new(big.Rat), avg: new(big.Rat), realized: new(big.Rat)}
		b.lots[k] = l
	}

	price := dec.FromProto(fill.GetPrice())
	signed := dec.FromProto(fill.GetQuantity())
	if fill.GetSide() == orderpb.Side_SIDE_SELL {
		signed = new(big.Rat).Neg(signed)
	}

	b.fold(l, signed, price)

	// Mark unrealized at the fill price: (price - avg) * qty.
	unreal := new(big.Rat).Mul(new(big.Rat).Sub(price, l.avg), l.qty)
	marketValue := new(big.Rat).Mul(price, l.qty)

	return &domainpb.PositionState{
		PortfolioId:   portfolioID,
		InstrumentId:  fill.GetInstrumentId(),
		Quantity:      dec.ToProto(l.qty),
		AveragePrice:  dec.ToProto(l.avg),
		MarketValue:   b.money(marketValue),
		RealizedPnl:   b.money(l.realized),
		UnrealizedPnl: b.money(unreal),
		AsOf:          timestamppb.New(asOf.UTC()),
	}
}

// fold mutates the lot by a signed fill quantity at price, applying
// weighted-average-cost accounting with realized P&L.
func (b *Book) fold(l *lot, signed, price *big.Rat) {
	cur := l.qty.Sign()
	add := signed.Sign()

	// Flat or same-direction: increase the position, re-average the cost.
	if cur == 0 || cur == add {
		oldAbs := new(big.Rat).Abs(l.qty)
		addAbs := new(big.Rat).Abs(signed)
		newQty := new(big.Rat).Add(l.qty, signed)
		newAbs := new(big.Rat).Abs(newQty)
		// new avg = (oldAbs*avg + addAbs*price) / newAbs
		cost := new(big.Rat).Mul(oldAbs, l.avg)
		cost.Add(cost, new(big.Rat).Mul(addAbs, price))
		l.avg = new(big.Rat).Quo(cost, newAbs)
		l.qty = newQty
		return
	}

	// Opposite direction: realize P&L on the closed portion.
	closeAbs := new(big.Rat).Abs(signed)
	openAbs := new(big.Rat).Abs(l.qty)
	if closeAbs.Cmp(openAbs) > 0 {
		closeAbs = openAbs // cross through zero; only the open portion is closed
	}
	// realized += closeAbs * (price - avg) * sign(currentQty)
	pnl := new(big.Rat).Mul(closeAbs, new(big.Rat).Sub(price, l.avg))
	if cur < 0 {
		pnl.Neg(pnl) // short: profit when price < avg
	}
	l.realized.Add(l.realized, pnl)

	newQty := new(big.Rat).Add(l.qty, signed)
	switch newQty.Sign() {
	case 0:
		l.qty = new(big.Rat)
		l.avg = new(big.Rat)
	default:
		if newQty.Sign() != cur {
			// Crossed through zero: a fresh lot opens at the fill price.
			l.avg = new(big.Rat).Set(price)
		}
		l.qty = newQty
	}
}

func (b *Book) money(r *big.Rat) *commonpb.Money {
	return &commonpb.Money{Amount: dec.ToProto(r), CurrencyCode: b.baseCcy}
}

// Snapshot returns a portfolio's current holdings as a PortfolioSnapshot — the
// read side the COMP-01 pre-trade gate projects an order onto (its BookSource).
// Positions are marked at average cost (no market mark is wired here); NAV is
// the net market value of the holdings, a funded-book proxy until a cash/equity
// source lands. Empty portfolio ⇒ a snapshot with no positions.
func (b *Book) Snapshot(portfolioID string, asOf time.Time) *domainpb.PortfolioSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	ts := timestamppb.New(asOf.UTC())
	nav := new(big.Rat)
	var positions []*domainpb.PositionState
	for k, l := range b.lots {
		if k.portfolio != portfolioID {
			continue
		}
		mv := new(big.Rat).Mul(l.avg, l.qty)
		nav.Add(nav, mv)
		positions = append(positions, &domainpb.PositionState{
			PortfolioId:  k.portfolio,
			InstrumentId: k.instrument,
			Quantity:     dec.ToProto(l.qty),
			AveragePrice: dec.ToProto(l.avg),
			MarketValue:  b.money(mv),
			RealizedPnl:  b.money(l.realized),
			AsOf:         ts,
		})
	}
	return &domainpb.PortfolioSnapshot{
		Portfolio: &domainpb.PortfolioState{
			PortfolioId:      portfolioID,
			BaseCurrency:     b.baseCcy,
			TotalMarketValue: b.money(nav),
			PositionCount:    uint32(len(positions)),
			AsOf:             ts,
		},
		Positions: positions,
	}
}

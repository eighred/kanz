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
	"context"
	"fmt"
	"math/big"
	"sync"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/costbasis"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
)

// key: a holding is identified by WHERE it sits, not just what it is (EXEC-M19a).
type key struct{ portfolio, venue, instrument string }

// lot is the running state of one holding, and is the PLATFORM'S lot rather than
// this package's (#428): signed quantity, the average cost of the open position
// (always non-negative), and cumulative realized P&L.
//
// An alias, not a copy. The arithmetic that maintains it moved to
// internal/costbasis, because three packages were each folding fills into their
// own version of this struct with their own copy of the same calculation — and
// they had drifted. The local name stays so this package still reads in its own
// vocabulary.
type lot = costbasis.Lot

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
//
// IN-PROCESS, AND THEREFORE CORRECT FOR EXACTLY ONE REPLICA. It cannot fail and it
// cannot be shared: two pods are two maps, each folding the fills its consumer group
// handed it, each publishing an ABSOLUTE position built from a fraction of the trades
// (EXEC-M18). Use Postgres in any deployment that runs more than one pod — which the
// shipped one does. The ctx and error exist to satisfy Store; neither is used here.
func (b *Book) Apply(_ context.Context, portfolioID string, fill *orderpb.Fill, asOf time.Time) (*Applied, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// THE SAME VALIDATION THE POSTGRES BOOK USES (#631). This one checked venue
	// and quantity and NOT fill_id, so the in-memory book folded a fill the
	// durable book parked — two implementations of one contract inside a single
	// package, disagreeing. It gains the identity check by sharing the rule.
	if err := fillfact.Validate(fill); err != nil {
		return nil, err
	}
	k := key{portfolioID, fill.GetVenue(), fill.GetInstrumentId()}
	l := b.lots[k]
	if l == nil {
		l = zeroLot()
		b.lots[k] = l
	}

	price := dec.FromProto(fill.GetPrice())
	signed := dec.FromProto(fill.GetQuantity())
	if fill.GetSide() == orderpb.Side_SIDE_SELL {
		signed = new(big.Rat).Neg(signed)
	}

	costbasis.Fold(l, signed, price)

	venueState, err := b.stateOf(portfolioID, fill.GetVenue(), fill.GetInstrumentId(), l, price, asOf)
	if err != nil {
		return nil, err
	}
	aggState, err := b.stateOf(portfolioID, "", fill.GetInstrumentId(), b.aggregate(portfolioID, fill.GetInstrumentId()), price, asOf)
	if err != nil {
		return nil, err
	}
	return &Applied{Venue: venueState, Aggregate: aggState}, nil
}

// aggregate sums every venue's holding of one instrument into the fund's position — the
// same cost-weighted fold the durable store does in SQL. Caller holds b.mu.
func (b *Book) aggregate(portfolioID, instrument string) *lot {
	agg := costbasis.NewAggregator()
	for k, l := range b.lots {
		if k.portfolio != portfolioID || k.instrument != instrument {
			continue
		}
		agg.Add(l)
	}
	return agg.Lot()
}

// stateOf marks at the fill price (the latest trade). An empty venue is the fund-level
// aggregate: it belongs to no single exchange.
func (b *Book) stateOf(portfolioID, venue, instrument string, l *lot, price *big.Rat, asOf time.Time) (*domainpb.PositionState, error) {
	unreal := new(big.Rat).Mul(new(big.Rat).Sub(price, l.AvgCost), l.Qty)
	qty, ok := dec.ToProtoScaled(l.Qty)
	if !ok {
		return nil, fmt.Errorf("position %s/%s: quantity is not representable", portfolioID, instrument)
	}
	avg, ok := dec.ToProtoScaled(l.AvgCost)
	if !ok {
		return nil, fmt.Errorf("position %s/%s: average price is not representable", portfolioID, instrument)
	}
	marketValue, err := b.money(new(big.Rat).Mul(price, l.Qty))
	if err != nil {
		return nil, fmt.Errorf("position %s/%s market value: %w", portfolioID, instrument, err)
	}
	realized, err := b.money(l.Realized)
	if err != nil {
		return nil, fmt.Errorf("position %s/%s realized pnl: %w", portfolioID, instrument, err)
	}
	unrealized, err := b.money(unreal)
	if err != nil {
		return nil, fmt.Errorf("position %s/%s unrealized pnl: %w", portfolioID, instrument, err)
	}
	return &domainpb.PositionState{
		PortfolioId:   portfolioID,
		Venue:         venue,
		InstrumentId:  instrument,
		Quantity:      qty,
		AveragePrice:  avg,
		MarketValue:   marketValue,
		RealizedPnl:   realized,
		UnrealizedPnl: unrealized,
		AsOf:          timestamppb.New(asOf.UTC()),
	}, nil
}

// money wraps an exact amount as Money, refusing rather than fabricating one.
// dec.ToProtoScaled preserves magnitude by rescaling, so a $100bn position value
// is represented at a coarser exponent rather than wrapped into a small number.
//
// THE !ok BRANCH IS EFFECTIVELY UNREACHABLE, AND IS KEPT ON PURPOSE. Refusal
// requires a value ToProtoScaled cannot represent at ANY exponent it will
// reach — around two billion decimal digits, which no position value approaches
// and no test can construct in finite time. It stays because the contract
// returns `ok` and discarding it is how the wrapping bug got in: the compiler is
// the only thing that keeps a future edit from ignoring it. Do not "simplify" it
// away on the grounds that it never fires.
//
// Returning a zero Money instead of an error would be worse than returning
// nothing: heldPositions treats a zero-valued position as flat and drops it, so
// the compliance rules would stop seeing the holding entirely (COMP-M1).
func (b *Book) money(r *big.Rat) (*commonpb.Money, error) {
	amt, ok := dec.ToProtoScaled(r)
	if !ok {
		return nil, fmt.Errorf("amount is not representable as a Decimal")
	}
	return &commonpb.Money{Amount: amt, CurrencyCode: b.baseCcy}, nil
}

// Snapshot returns a portfolio's current holdings as a PortfolioSnapshot — the
// read side the COMP-01 pre-trade gate projects an order onto (its BookSource).
// Positions are marked at average cost (no market mark is wired here); NAV is
// the net market value of the holdings, a funded-book proxy until a cash/equity
// source lands. Empty portfolio ⇒ a snapshot with no positions.
func (b *Book) Snapshot(_ context.Context, portfolioID string, asOf time.Time) (*domainpb.PortfolioSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ts := timestamppb.New(asOf.UTC())
	nav := new(big.Rat)
	var positions []*domainpb.PositionState

	// AGGREGATED ACROSS VENUES. BTC held at two exchanges is ONE instrument to a
	// concentration limit; handing the gate two rows would let it check a limit against a
	// fraction of the fund's actual holding.
	seen := map[string]bool{}
	for k := range b.lots {
		if k.portfolio != portfolioID || seen[k.instrument] {
			continue
		}
		seen[k.instrument] = true
		l := b.aggregate(portfolioID, k.instrument)
		mv := new(big.Rat).Mul(l.AvgCost, l.Qty)
		nav.Add(nav, mv)
		marketValue, err := b.money(mv)
		if err != nil {
			return nil, fmt.Errorf("portfolio %s position %s: %w", portfolioID, k.instrument, err)
		}
		realized, err := b.money(l.Realized)
		if err != nil {
			return nil, fmt.Errorf("portfolio %s position %s realized pnl: %w", portfolioID, k.instrument, err)
		}
		qty, ok := dec.ToProtoScaled(l.Qty)
		if !ok {
			return nil, fmt.Errorf("portfolio %s position %s: quantity is not representable", portfolioID, k.instrument)
		}
		avg, ok := dec.ToProtoScaled(l.AvgCost)
		if !ok {
			return nil, fmt.Errorf("portfolio %s position %s: average price is not representable", portfolioID, k.instrument)
		}
		positions = append(positions, &domainpb.PositionState{
			PortfolioId:  portfolioID,
			InstrumentId: k.instrument,
			Quantity:     qty,
			AveragePrice: avg,
			MarketValue:  marketValue,
			RealizedPnl:  realized,
			AsOf:         ts,
		})
	}
	navMoney, err := b.money(nav)
	if err != nil {
		return nil, fmt.Errorf("portfolio %s NAV: %w", portfolioID, err)
	}
	return &domainpb.PortfolioSnapshot{
		Portfolio: &domainpb.PortfolioState{
			PortfolioId:      portfolioID,
			BaseCurrency:     b.baseCcy,
			TotalMarketValue: navMoney,
			PositionCount:    uint32(len(positions)),
			AsOf:             ts,
		},
		Positions: positions,
	}, nil
}

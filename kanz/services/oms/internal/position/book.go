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
	"github.com/eighred/kanz/internal/outbox"
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
	queue   *outbox.Memory
	// appliedFills is position_fills (#818): the fills already folded into these
	// lots, keyed on fill_id — the same key the table's PRIMARY KEY carries once
	// the tenant column is dropped, and this book belongs to exactly one tenant
	// (NewProjector refuses an empty one) so there is nothing to scope it by.
	//
	// Held under the SAME b.mu as the lots and the outbox, which is this store's
	// whole equivalent of the transaction Postgres.Apply opens. A claim that
	// could commit apart from the fold has two failure modes and both are real.
	//
	// IT EXISTS BECAUSE THIS IS THE TEST SEAM. Every service-level test of the
	// projector runs against this book, so without the claim a redelivery test
	// certified exactly-once folding against a store that doubled the position —
	// the same divergence order.MemoryStore.appliedFills was added to end.
	//
	// IT GROWS WITHOUT BOUND, AND SO DOES position_fills. Exactly-once over an
	// unbounded stream of fills costs one key per fill in either backend; the
	// durable one pays it in a table an operator can see and archive, this one in
	// a map that dies with the process. That is a real cost of this store rather
	// than a defect of the claim, and it is one more reason the in-memory book is
	// a single-replica seam and not a deployment.
	appliedFills map[string]bool
}

// BookOption configures the in-memory book.
type BookOption func(*Book)

// WithSharedOutbox makes this book announce into a queue somebody else owns —
// in practice the order store's, so the ONE relay the composition root already
// runs drains both (#795).
//
// WITHOUT IT THE IN-MEMORY BOOK HAS A QUEUE NOTHING DRAINS. The durable store
// gets a background drainer for free: it writes the same outbox TABLE the order
// store does, over the same pool, so the relay the OMS already runs finds its
// records. Two in-process maps have no such shared table, and a record whose
// only drainer is the inline flush is a FACT that survives exactly as long as
// the bus keeps redelivering the fill.
func WithSharedOutbox(q *outbox.Memory) BookOption {
	return func(b *Book) {
		if q != nil {
			b.queue = q
		}
	}
}

// NewBook returns an empty book stamping Money in baseCcy.
func NewBook(baseCcy string, opts ...BookOption) *Book {
	if baseCcy == "" {
		baseCcy = "USD"
	}
	b := &Book{
		lots:         make(map[key]*lot),
		appliedFills: make(map[string]bool),
		baseCcy:      baseCcy,
		queue:        outbox.NewMemory(),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Outbox is the in-process queue this book enqueues announced FACTs into.
func (b *Book) Outbox() outbox.Queue { return b.queue }

// Apply folds one fill into the book EXACTLY ONCE and returns the resulting
// PositionState. A fill this book has already folded is claimed-and-skipped, and
// the book as it stands is returned and announced — the same two steps, in the
// same order, that Postgres.Apply takes around its position_fills claim.
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
func (b *Book) Apply(ctx context.Context, portfolioID string, fill *orderpb.Fill, asOf time.Time, announce Announcer) (*Applied, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// THE SAME VALIDATION THE POSTGRES BOOK USES (#631). This one checked venue
	// and quantity and NOT fill_id, so the in-memory book folded a fill the
	// durable book parked — two implementations of one contract inside a single
	// package, disagreeing. It gains the identity check by sharing the rule.
	if err := fillfact.Validate(fill); err != nil {
		return nil, err
	}
	// THE CLAIM BEFORE THE FOLD, WHICH IS WHAT position_fills DOES (#818). The
	// Store contract this type satisfies says a fill already folded — by a
	// redelivery, or by another pod — is not counted again; the durable book
	// honoured that with an INSERT ... ON CONFLICT DO NOTHING whose RowsAffected
	// it read, and this one honoured it not at all. Two implementations of one
	// contract inside a single package, disagreeing about the answer that
	// matters most: how much the fund holds.
	//
	// A double-counted fill does not fail. It publishes an ABSOLUTE
	// PositionState on a compacted subject, and the risk engine, the compliance
	// monitor and the OMS's own pre-trade gate all admit orders against it.
	claimed := !b.appliedFills[fill.GetFillId()]
	if claimed {
		b.appliedFills[fill.GetFillId()] = true
	}

	k := key{portfolioID, fill.GetVenue(), fill.GetInstrumentId()}
	l := b.lots[k]
	if l == nil {
		l = zeroLot()
		// A SKIPPED FOLD LEAVES NO HOLDING BEHIND, mirroring the durable store,
		// which upserts `positions` only inside the same branch as the fold. A
		// zero lot recorded here would surface from Snapshot as a position the
		// fund does not hold.
		if claimed {
			b.lots[k] = l
		}
	}

	price := dec.FromProto(fill.GetPrice())
	if claimed {
		signed := dec.FromProto(fill.GetQuantity())
		if fill.GetSide() == orderpb.Side_SIDE_SELL {
			signed = new(big.Rat).Neg(signed)
		}
		costbasis.Fold(l, signed, price)
	}

	venueState, err := b.stateOf(portfolioID, fill.GetVenue(), fill.GetInstrumentId(), l, price, asOf)
	if err != nil {
		return nil, err
	}
	aggState, err := b.stateOf(portfolioID, "", fill.GetInstrumentId(), b.aggregate(portfolioID, fill.GetInstrumentId()), price, asOf)
	if err != nil {
		return nil, err
	}
	applied := &Applied{Venue: venueState, Aggregate: aggState}

	// THE ANNOUNCEMENT HAPPENS UNDER b.mu, which is this store's whole equivalent
	// of the transaction the durable one opens — the same argument
	// order.MemoryStore.Create makes for enqueuing inside its lock hold. A
	// permissive double certifies behaviour production does not have, and the
	// behaviour under test here is that the FACT cannot be built from a fold
	// another goroutine has already moved past.
	if announce != nil {
		records, aerr := announce(ctx, applied)
		if aerr != nil {
			return nil, aerr
		}
		if err := b.queue.Append(records...); err != nil {
			return nil, err
		}
	}
	return applied, nil
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

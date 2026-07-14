package position

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/internal/dec"
)

// Store is the position book. Memory (*Book) is correct for exactly ONE replica;
// Postgres is correct across all of them.
//
// The distinction is not a scaling nicety. The projector PUBLISHES the result of a fold
// as an ABSOLUTE PositionState, and the OMS's pre-trade gate PROJECTS EVERY ORDER onto
// the book before admitting it — so a book that has seen only half the fills does not
// fail, it asserts a wrong number and admits an order against a limit it cannot see
// (EXEC-M18).
type Store interface {
	// Apply folds one fill EXACTLY ONCE and returns the resulting absolute position.
	// A fill already folded — by a redelivery, or by another pod — is not counted
	// again; the current state is returned unchanged.
	Apply(ctx context.Context, portfolioID string, fill *orderpb.Fill, asOf time.Time) (*domainpb.PositionState, error)
	// Snapshot is the read side the pre-trade gate projects an order onto.
	Snapshot(ctx context.Context, portfolioID string, asOf time.Time) (*domainpb.PortfolioSnapshot, error)
}

// ErrFillNotIdentified: a fill with no fill_id cannot be deduplicated, so folding it
// would risk counting the same trade twice — once per pod, or again on a redelivery.
// Every venue stamps one (Binance "symbol-tradeID", OKX "instId-tradeId", the sim
// generates one), so an empty id is a defect in the producer, not a case to tolerate.
// Silently double-counting a fill is how a fund's position drifts from the exchange's.
var ErrFillNotIdentified = errors.New("position: fill has no fill_id and cannot be folded exactly once")

// Postgres is the durable, cross-pod position book.
type Postgres struct {
	pool    *pgxpool.Pool
	baseCcy string
}

// NewPostgres returns the durable store. baseCcy stamps Money, as the in-memory Book does.
func NewPostgres(pool *pgxpool.Pool, baseCcy string) *Postgres {
	if baseCcy == "" {
		baseCcy = "USD"
	}
	return &Postgres{pool: pool, baseCcy: baseCcy}
}

var _ Store = (*Postgres)(nil)
var _ Store = (*Book)(nil)

// Apply folds one fill in a single transaction.
//
//  1. Claim the fill (INSERT ... ON CONFLICT DO NOTHING). RowsAffected 0 ⇒ somebody
//     already folded it — another pod, or an earlier delivery of the same event — so
//     the fold is SKIPPED and the current position returned. This is the same
//     engine-side exactly-once stance the order store's admission gate takes, and for
//     the same reason: a SELECT-then-INSERT is a check-then-act, and the window between
//     them is where a trade gets counted twice.
//  2. Lock the lot row (FOR UPDATE), so two pods folding the same instrument serialize
//     at the engine instead of racing in two address spaces.
//  3. Apply the SAME weighted-average-cost fold the in-memory book uses.
//  4. Upsert, commit.
func (p *Postgres) Apply(ctx context.Context, portfolioID string, fill *orderpb.Fill, asOf time.Time) (*domainpb.PositionState, error) {
	if fill.GetFillId() == "" {
		return nil, ErrFillNotIdentified
	}
	instrument := fill.GetInstrumentId()

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("position: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful Commit

	claim, err := tx.Exec(ctx,
		`INSERT INTO position_fills (fill_id) VALUES ($1) ON CONFLICT DO NOTHING`, fill.GetFillId())
	if err != nil {
		return nil, fmt.Errorf("position: claim fill %s: %w", fill.GetFillId(), err)
	}

	l, err := loadLot(ctx, tx, portfolioID, instrument)
	if err != nil {
		return nil, err
	}

	// Already folded by somebody else: return the book as it stands, do NOT count it again.
	if claim.RowsAffected() == 0 {
		return p.stateOf(portfolioID, instrument, l, dec.FromProto(fill.GetPrice()), asOf), nil
	}

	price := dec.FromProto(fill.GetPrice())
	signed := dec.FromProto(fill.GetQuantity())
	if fill.GetSide() == orderpb.Side_SIDE_SELL {
		signed = new(big.Rat).Neg(signed)
	}
	foldLot(l, signed, price)

	if _, err := tx.Exec(ctx, `
		INSERT INTO positions (portfolio_id, instrument_id, quantity, average_price, realized_pnl)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, portfolio_id, instrument_id) DO UPDATE
		SET quantity = EXCLUDED.quantity,
		    average_price = EXCLUDED.average_price,
		    realized_pnl = EXCLUDED.realized_pnl,
		    updated_at = now()`,
		portfolioID, instrument, l.qty.RatString(), l.avg.RatString(), l.realized.RatString()); err != nil {
		return nil, fmt.Errorf("position: upsert %s/%s: %w", portfolioID, instrument, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("position: commit: %w", err)
	}
	return p.stateOf(portfolioID, instrument, l, price, asOf), nil
}

// loadLot reads the lot FOR UPDATE, returning a zero lot when the holding is new.
func loadLot(ctx context.Context, tx pgx.Tx, portfolioID, instrument string) (*lot, error) {
	var qty, avg, realized string
	err := tx.QueryRow(ctx, `
		SELECT quantity, average_price, realized_pnl
		FROM positions
		WHERE portfolio_id = $1 AND instrument_id = $2
		FOR UPDATE`, portfolioID, instrument).Scan(&qty, &avg, &realized)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return &lot{qty: new(big.Rat), avg: new(big.Rat), realized: new(big.Rat)}, nil
	case err != nil:
		return nil, fmt.Errorf("position: load %s/%s: %w", portfolioID, instrument, err)
	}
	l, err := lotFrom(qty, avg, realized)
	if err != nil {
		return nil, fmt.Errorf("position: %s/%s: %w", portfolioID, instrument, err)
	}
	return l, nil
}

// lotFrom parses the stored exact decimals. A value that will not parse is a corrupt
// book, never a zero — booking a fill onto a lot we could not read would invent a basis.
func lotFrom(qty, avg, realized string) (*lot, error) {
	l := &lot{}
	for _, f := range []struct {
		s   string
		dst **big.Rat
	}{{qty, &l.qty}, {avg, &l.avg}, {realized, &l.realized}} {
		r, ok := new(big.Rat).SetString(f.s)
		if !ok {
			return nil, fmt.Errorf("unparseable stored decimal %q", f.s)
		}
		*f.dst = r
	}
	return l, nil
}

// Snapshot returns the portfolio's holdings — the read side the pre-trade gate projects
// an order onto. Marked at average cost, as the in-memory book does.
func (p *Postgres) Snapshot(ctx context.Context, portfolioID string, asOf time.Time) (*domainpb.PortfolioSnapshot, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT instrument_id, quantity, average_price, realized_pnl
		FROM positions
		WHERE portfolio_id = $1
		ORDER BY instrument_id`, portfolioID)
	if err != nil {
		return nil, fmt.Errorf("position: snapshot %s: %w", portfolioID, err)
	}
	defer rows.Close()

	ts := timestamppb.New(asOf.UTC())
	nav := new(big.Rat)
	var positions []*domainpb.PositionState
	for rows.Next() {
		var instrument, qty, avg, realized string
		if err := rows.Scan(&instrument, &qty, &avg, &realized); err != nil {
			return nil, fmt.Errorf("position: scan %s: %w", portfolioID, err)
		}
		l, err := lotFrom(qty, avg, realized)
		if err != nil {
			return nil, fmt.Errorf("position: %s/%s: %w", portfolioID, instrument, err)
		}
		mv := new(big.Rat).Mul(l.avg, l.qty)
		nav.Add(nav, mv)
		positions = append(positions, &domainpb.PositionState{
			PortfolioId:  portfolioID,
			InstrumentId: instrument,
			Quantity:     dec.ToProto(l.qty),
			AveragePrice: dec.ToProto(l.avg),
			MarketValue:  p.money(mv),
			RealizedPnl:  p.money(l.realized),
			AsOf:         ts,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("position: snapshot %s: %w", portfolioID, err)
	}

	return &domainpb.PortfolioSnapshot{
		Portfolio: &domainpb.PortfolioState{
			PortfolioId:      portfolioID,
			BaseCurrency:     p.baseCcy,
			TotalMarketValue: p.money(nav),
			PositionCount:    uint32(len(positions)),
			AsOf:             ts,
		},
		Positions: positions,
	}, nil
}

// stateOf builds the published PositionState, marked at the fill price (the latest
// trade) — the same marking the in-memory book applies.
func (p *Postgres) stateOf(portfolioID, instrument string, l *lot, price *big.Rat, asOf time.Time) *domainpb.PositionState {
	unreal := new(big.Rat).Mul(new(big.Rat).Sub(price, l.avg), l.qty)
	return &domainpb.PositionState{
		PortfolioId:   portfolioID,
		InstrumentId:  instrument,
		Quantity:      dec.ToProto(l.qty),
		AveragePrice:  dec.ToProto(l.avg),
		MarketValue:   p.money(new(big.Rat).Mul(price, l.qty)),
		RealizedPnl:   p.money(l.realized),
		UnrealizedPnl: p.money(unreal),
		AsOf:          timestamppb.New(asOf.UTC()),
	}
}

func (p *Postgres) money(r *big.Rat) *commonpb.Money {
	return &commonpb.Money{Amount: dec.ToProto(r), CurrencyCode: p.baseCcy}
}

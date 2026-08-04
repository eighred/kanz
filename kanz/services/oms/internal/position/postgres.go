package position

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
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
	// Apply folds one fill EXACTLY ONCE and returns BOTH projections of the book. A fill
	// already folded — by a redelivery, or by another pod — is not counted again; the
	// current state is returned unchanged.
	Apply(ctx context.Context, portfolioID string, fill *orderpb.Fill, asOf time.Time) (*Applied, error)
	// Snapshot is the read side the pre-trade gate projects an order onto: the fund's
	// holdings AGGREGATED across venues — one row per instrument, because a concentration
	// limit is about the fund's BTC, not the BTC at one exchange.
	Snapshot(ctx context.Context, portfolioID string, asOf time.Time) (*domainpb.PortfolioSnapshot, error)
}

// Applied is ONE BOOK, PROJECTED TWICE (EXEC-M19a).
//
// Venue is the holding AT ONE VENUE — what the execution plane needs, because a CLOSE must
// flatten what each venue actually holds, and you cannot sell 1 BTC on Binance if it is
// sitting at OKX.
//
// Aggregate is the fund's position in that instrument, SUMMED across every venue — what the
// risk engine and the compliance monitor consume, because a fund's exposure does not care
// which exchange holds it.
//
// Both are true at once, and both are derived from the same durable rows in the same
// transaction. Storing the aggregate separately would be a second copy of the truth, and the
// day the two disagreed nobody could say which one the fund actually held.
type Applied struct {
	Venue     *domainpb.PositionState
	Aggregate *domainpb.PositionState
}

// ErrFillNotIdentified: a fill with no fill_id cannot be deduplicated, so folding it
// would risk counting the same trade twice — once per pod, or again on a redelivery.
// Every venue stamps one (Binance "symbol-tradeID", OKX "instId-tradeId", the sim
// generates one), so an empty id is a defect in the producer, not a case to tolerate.
// Silently double-counting a fill is how a fund's position drifts from the exchange's.
var ErrFillNotIdentified = errors.New("position: fill has no fill_id and cannot be folded exactly once")

// ErrFillHasNoVenue: a fill with no venue cannot be attributed to a holding.
//
// A position sits AT a venue — that is what makes it closable, and what makes its collateral
// segregable (EXEC-M16). order.v1.Fill has always carried the venue; a fill arriving without
// one is a producer defect, and folding it into a venue-less bucket would create a holding
// that no CLOSE could ever reach.
var ErrFillHasNoVenue = errors.New("position: fill has no venue — a holding that belongs to no exchange cannot be closed")

// ErrFillQuantityNotPositive: a fill that moved nothing is a producer defect, and
// folding it PANICS.
//
// foldLot's re-average divides by the new absolute quantity. For a lot this pod has
// not folded before (cur == 0) a zero-quantity fill leaves that divisor at zero, and
// big.Rat.Quo panics `division by zero` — reproduced, and it is where #217 came from.
// Nothing recovered it, the delivery was never acked, and the broker redelivered it
// into the replacement pod: one malformed fill FACT, and the OMS crash-looped
// estate-wide until someone removed the message by hand.
//
// The order aggregate has always rejected this input — aggregate.go's
// `fill quantity must be > 0`. The projector is a SECOND consumer of the same
// order.order.filled subject and did not, so the two disagreed about what a valid
// fill is. This is that guard, on the other path. dec.FromProto(nil) yields zero, so
// this also covers an ABSENT quantity, not just a literal 0.
//
// Refusing is right rather than skipping: a fill that moves no quantity carries no
// information a position book can use, and silently ignoring it would hide a
// misbehaving producer behind a healthy-looking consumer.
var ErrFillQuantityNotPositive = errors.New("position: fill quantity must be > 0 — a fill that moves nothing cannot be folded into a holding")

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

var (
	_ Store = (*Postgres)(nil)
	_ Store = (*Book)(nil)
)

// Apply folds one fill in a single transaction.
//
//  1. Take the (tenant, portfolio, instrument) advisory lock — FIRST, before any row
//     lock. It is what makes step 5's cross-venue SUM true; see lockInstrument, which
//     also explains why the FOR UPDATE in step 3 cannot do that job.
//  2. Claim the fill (INSERT ... ON CONFLICT DO NOTHING). RowsAffected 0 ⇒ somebody
//     already folded it — another pod, or an earlier delivery of the same event — so
//     the fold is SKIPPED and the current book returned. This is the same engine-side
//     exactly-once stance the order store's admission gate takes, and for the same reason:
//     a SELECT-then-INSERT is a check-then-act, and the window between them is where a
//     trade gets counted twice.
//  3. Lock the (portfolio, venue, instrument) lot FOR UPDATE. This is now a BACKSTOP for
//     any writer that reaches the positions table without step 1's lock, not the thing
//     the aggregate rests on.
//  4. Apply the SAME weighted-average-cost fold the in-memory book uses.
//  5. Upsert, then SUM the venues into the fund-level position — in the same transaction
//     that just changed one of them, and under the lock from step 1, so the aggregate
//     cannot disagree with the rows it came from.
func (p *Postgres) Apply(ctx context.Context, portfolioID string, fill *orderpb.Fill, asOf time.Time) (*Applied, error) {
	if fill.GetFillId() == "" {
		return nil, ErrFillNotIdentified
	}
	if fill.GetVenue() == "" {
		return nil, ErrFillHasNoVenue
	}
	if !dec.IsPositive(fill.GetQuantity()) {
		return nil, ErrFillQuantityNotPositive
	}
	venue, instrument := fill.GetVenue(), fill.GetInstrumentId()
	price := dec.FromProto(fill.GetPrice())

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("position: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful Commit

	if err := lockInstrument(ctx, tx, portfolioID, instrument); err != nil {
		return nil, err
	}

	claim, err := tx.Exec(ctx,
		`INSERT INTO position_fills (fill_id) VALUES ($1) ON CONFLICT DO NOTHING`, fill.GetFillId())
	if err != nil {
		return nil, fmt.Errorf("position: claim fill %s: %w", fill.GetFillId(), err)
	}

	l, err := loadLot(ctx, tx, portfolioID, venue, instrument)
	if err != nil {
		return nil, err
	}

	// Already folded by somebody else: return the book as it stands, do NOT count it again.
	if claim.RowsAffected() > 0 {
		signed := dec.FromProto(fill.GetQuantity())
		if fill.GetSide() == orderpb.Side_SIDE_SELL {
			signed = new(big.Rat).Neg(signed)
		}
		foldLot(l, signed, price)

		if _, err := tx.Exec(ctx, `
			INSERT INTO positions (portfolio_id, venue, instrument_id, quantity, average_price, realized_pnl)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (tenant_id, portfolio_id, venue, instrument_id) DO UPDATE
			SET quantity = EXCLUDED.quantity,
			    average_price = EXCLUDED.average_price,
			    realized_pnl = EXCLUDED.realized_pnl,
			    updated_at = now()`,
			portfolioID, venue, instrument, l.qty.RatString(), l.avg.RatString(), l.realized.RatString()); err != nil {
			return nil, fmt.Errorf("position: upsert %s/%s/%s: %w", portfolioID, venue, instrument, err)
		}
	}

	agg, err := aggregateLot(ctx, tx, portfolioID, instrument)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("position: commit: %w", err)
	}

	venueState, err := p.stateOf(portfolioID, venue, instrument, l, price, asOf)
	if err != nil {
		return nil, err
	}
	aggState, err := p.stateOf(portfolioID, "", instrument, agg, price, asOf)
	if err != nil {
		return nil, err
	}
	return &Applied{Venue: venueState, Aggregate: aggState}, nil
}

// lockInstrument serializes every fold that can move one instrument's FUND-LEVEL
// aggregate: the whole of (tenant, portfolio, instrument), ACROSS VENUES and across pods.
//
// # Why the row lock is not enough (#226)
//
// loadLot takes FOR UPDATE on ONE VENUE'S ROW. aggregateLot then SUMs EVERY VENUE'S ROW —
// rows that lock does not cover. Under READ COMMITTED, two fills at two venues folding at
// once each upsert their own row and then sum the other venue at its PRE-FILL value: both
// published aggregates are short by the other fill. oms-deploy.yaml runs `replicas: 2` and
// order/service.go dispatches from three goroutines, so this is the normal path.
//
// It is the worst shape a discrepancy can take here, because nothing reconciles it. The
// positions rows stay CORRECT — only the published number is wrong — and projector.go
// publishes the aggregate as an ABSOLUTE PositionState on a COMPACTED subject, so the short
// value is RETAINED. The risk engine, the compliance monitor and tv-sync then read a fund
// exposure short by one fill until the next fill in that instrument happens to arrive.
//
// # Why an advisory lock rather than a wider FOR UPDATE
//
// Widening the FOR UPDATE to the portfolio+instrument predicate does NOT close it. FOR
// UPDATE can only lock rows that ALREADY EXIST, and the first fill at a second venue has no
// row yet — two brand-new venue holdings would lock nothing and race exactly as before.
// Below SERIALIZABLE, Postgres has no predicate lock; an advisory lock is the only mutual
// exclusion that also covers a row that is about to be created.
//
// # Why not raise the isolation level
//
// Two reasons, and the second is fatal. A 40001 has to be RETRIED by somebody: no bus
// consumer in this estate wires bus.WithRetry, MaxAttempts is 1 (pkg/bus/retry.go), so a
// serialization failure escaping Projector.Handle DLQs the fill. #220 gave the DLQ a drain;
// routine write contention is not what a drain is for. And REPEATABLE READ would break THIS
// design outright — it pins the transaction snapshot at the FIRST statement, which is this
// lock, so the loser would acquire the lock and then still read the pre-commit snapshot.
// READ COMMITTED's per-statement snapshot is what lets the loser see the winner's committed
// rows. It is load-bearing: do not "harden" the isolation level without re-reading this.
//
// # Lock ordering
//
// This is the FIRST lock Apply takes — before the position_fills claim, before any FOR
// UPDATE — and the only one taken outside the (tenant, portfolio, instrument) class it
// names. Every transaction therefore acquires in the same order, and two transactions
// holding different advisory locks never contend for the same positions row, so widening
// the lock cannot deadlock. Taking any lock ABOVE this one would end that.
//
// The key is hashed, so two unrelated instruments can collide and serialize with each
// other. That costs throughput and nothing else — a hash collision can over-lock, never
// under-lock. The TWO-INT form occupies a lock space distinct from the int8 form, so these
// can never collide with internal/migrate's runner lock or linkstore's chain lock.
//
// The tenant is in the key because an advisory lock is CLUSTER-GLOBAL — it knows nothing
// about RLS — and two tenants owning a portfolio of the same name must not serialize against
// each other. app_current_tenant() RAISES on an unscoped session (MT-01e), so a connection
// that never set the GUC fails HERE, loudly, rather than taking a lock silently shared by
// every tenant.
//
// Unlike the DEFAULT on positions.tenant_id — whose function OID is resolved once at CREATE
// TABLE — this is a RUNTIME call and so resolves through the session's search_path. Nothing
// in this estate sets one (internal/pg.NewTenantPool leaves the DSN default), which is why
// it finds the function migration 0002 installs. A DSN that pinned a search_path excluding
// that schema would break this on the FIRST fill, loudly and immediately, which is the
// failure direction this repo asks for — but it is a coupling, so it is written down.
func lockInstrument(ctx context.Context, tx pgx.Tx, portfolioID, instrument string) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext(app_current_tenant() || '/' || $1), hashtext($2))`,
		portfolioID, instrument); err != nil {
		return fmt.Errorf("position: lock %s/%s: %w", portfolioID, instrument, err)
	}
	return nil
}

// loadLot reads one venue's lot FOR UPDATE, returning a zero lot when the holding is new.
//
// The FOR UPDATE is a BACKSTOP, not the guarantee the fund-level aggregate rests on — it
// covers one venue's row, and lockInstrument covers the set aggregateLot actually reads.
// It is kept because it still serializes any future writer to this table that reaches it
// without taking the advisory lock; it earns nothing against Apply, which always holds it.
func loadLot(ctx context.Context, tx pgx.Tx, portfolioID, venue, instrument string) (*lot, error) {
	var qty, avg, realized string
	err := tx.QueryRow(ctx, `
		SELECT quantity, average_price, realized_pnl
		FROM positions
		WHERE portfolio_id = $1 AND venue = $2 AND instrument_id = $3
		FOR UPDATE`, portfolioID, venue, instrument).Scan(&qty, &avg, &realized)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return zeroLot(), nil
	case err != nil:
		return nil, fmt.Errorf("position: load %s/%s/%s: %w", portfolioID, venue, instrument, err)
	}
	l, err := lotFrom(qty, avg, realized)
	if err != nil {
		return nil, fmt.Errorf("position: %s/%s/%s: %w", portfolioID, venue, instrument, err)
	}
	return l, nil
}

// aggregateLot sums every venue's holding of one instrument into the FUND's position.
//
// Quantity and realized P&L add. The average price is COST-WEIGHTED across venues: the
// fund's basis in BTC is what it paid for all of it, not what it paid at whichever exchange
// filled last. A fund that is flat in the instrument (every venue closed) has no basis, and
// reports none rather than a division by zero.
func aggregateLot(ctx context.Context, tx pgx.Tx, portfolioID, instrument string) (*lot, error) {
	rows, err := tx.Query(ctx, `
		SELECT quantity, average_price, realized_pnl
		FROM positions
		WHERE portfolio_id = $1 AND instrument_id = $2`, portfolioID, instrument)
	if err != nil {
		return nil, fmt.Errorf("position: aggregate %s/%s: %w", portfolioID, instrument, err)
	}
	defer rows.Close()

	agg := zeroLot()
	cost, absQty := new(big.Rat), new(big.Rat)
	for rows.Next() {
		var q, a, r string
		if err := rows.Scan(&q, &a, &r); err != nil {
			return nil, fmt.Errorf("position: aggregate scan %s/%s: %w", portfolioID, instrument, err)
		}
		l, err := lotFrom(q, a, r)
		if err != nil {
			return nil, fmt.Errorf("position: %s/%s: %w", portfolioID, instrument, err)
		}
		agg.qty.Add(agg.qty, l.qty)
		agg.realized.Add(agg.realized, l.realized)
		abs := new(big.Rat).Abs(l.qty)
		absQty.Add(absQty, abs)
		cost.Add(cost, new(big.Rat).Mul(abs, l.avg))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("position: aggregate %s/%s: %w", portfolioID, instrument, err)
	}
	if absQty.Sign() != 0 {
		agg.avg = new(big.Rat).Quo(cost, absQty)
	}
	return agg, nil
}

func zeroLot() *lot {
	return &lot{qty: new(big.Rat), avg: new(big.Rat), realized: new(big.Rat)}
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

// Snapshot returns the portfolio's holdings AGGREGATED ACROSS VENUES — the read side the
// pre-trade gate projects an order onto. BTC held at two exchanges is ONE instrument to a
// concentration limit; giving the gate two rows would let it check a limit against a
// fraction of the fund's actual holding. Marked at average cost, as the in-memory book does.
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

	// instrument → the fund's summed lot, plus the cost/abs-qty running totals the
	// cost-weighted average price needs.
	type acc struct {
		l      *lot
		cost   *big.Rat
		absQty *big.Rat
	}
	byInstrument := map[string]*acc{}
	var order []string

	for rows.Next() {
		var instrument, q, a, r string
		if err := rows.Scan(&instrument, &q, &a, &r); err != nil {
			return nil, fmt.Errorf("position: scan %s: %w", portfolioID, err)
		}
		l, err := lotFrom(q, a, r)
		if err != nil {
			return nil, fmt.Errorf("position: %s/%s: %w", portfolioID, instrument, err)
		}
		x := byInstrument[instrument]
		if x == nil {
			x = &acc{l: zeroLot(), cost: new(big.Rat), absQty: new(big.Rat)}
			byInstrument[instrument] = x
			order = append(order, instrument)
		}
		x.l.qty.Add(x.l.qty, l.qty)
		x.l.realized.Add(x.l.realized, l.realized)
		abs := new(big.Rat).Abs(l.qty)
		x.absQty.Add(x.absQty, abs)
		x.cost.Add(x.cost, new(big.Rat).Mul(abs, l.avg))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("position: snapshot %s: %w", portfolioID, err)
	}

	ts := timestamppb.New(asOf.UTC())
	nav := new(big.Rat)
	positions := make([]*domainpb.PositionState, 0, len(order))
	for _, instrument := range order {
		x := byInstrument[instrument]
		if x.absQty.Sign() != 0 {
			x.l.avg = new(big.Rat).Quo(x.cost, x.absQty)
		}
		mv := new(big.Rat).Mul(x.l.avg, x.l.qty)
		nav.Add(nav, mv)
		marketValue, err := p.money(mv)
		if err != nil {
			return nil, fmt.Errorf("portfolio %s position %s: %w", portfolioID, instrument, err)
		}
		realized, err := p.money(x.l.realized)
		if err != nil {
			return nil, fmt.Errorf("portfolio %s position %s realized pnl: %w", portfolioID, instrument, err)
		}
		qty, ok := dec.ToProtoScaled(x.l.qty)
		if !ok {
			return nil, fmt.Errorf("portfolio %s position %s: quantity is not representable", portfolioID, instrument)
		}
		avg, ok := dec.ToProtoScaled(x.l.avg)
		if !ok {
			return nil, fmt.Errorf("portfolio %s position %s: average price is not representable", portfolioID, instrument)
		}
		positions = append(positions, &domainpb.PositionState{
			PortfolioId:  portfolioID,
			InstrumentId: instrument,
			Quantity:     qty,
			AveragePrice: avg,
			MarketValue:  marketValue,
			RealizedPnl:  realized,
			AsOf:         ts,
		})
	}

	navMoney, err := p.money(nav)
	if err != nil {
		return nil, fmt.Errorf("portfolio %s NAV: %w", portfolioID, err)
	}
	return &domainpb.PortfolioSnapshot{
		Portfolio: &domainpb.PortfolioState{
			PortfolioId:      portfolioID,
			BaseCurrency:     p.baseCcy,
			TotalMarketValue: navMoney,
			PositionCount:    uint32(len(positions)),
			AsOf:             ts,
		},
		Positions: positions,
	}, nil
}

// stateOf builds a published PositionState, marked at the fill price (the latest trade) —
// the same marking the in-memory book applies. An empty venue means the fund-level
// aggregate: it belongs to no single exchange.
func (p *Postgres) stateOf(portfolioID, venue, instrument string, l *lot, price *big.Rat, asOf time.Time) (*domainpb.PositionState, error) {
	unreal := new(big.Rat).Mul(new(big.Rat).Sub(price, l.avg), l.qty)
	qty, ok := dec.ToProtoScaled(l.qty)
	if !ok {
		return nil, fmt.Errorf("position %s/%s: quantity is not representable", portfolioID, instrument)
	}
	avg, ok := dec.ToProtoScaled(l.avg)
	if !ok {
		return nil, fmt.Errorf("position %s/%s: average price is not representable", portfolioID, instrument)
	}
	marketValue, err := p.money(new(big.Rat).Mul(price, l.qty))
	if err != nil {
		return nil, fmt.Errorf("position %s/%s market value: %w", portfolioID, instrument, err)
	}
	realized, err := p.money(l.realized)
	if err != nil {
		return nil, fmt.Errorf("position %s/%s realized pnl: %w", portfolioID, instrument, err)
	}
	unrealized, err := p.money(unreal)
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
func (p *Postgres) money(r *big.Rat) (*commonpb.Money, error) {
	amt, ok := dec.ToProtoScaled(r)
	if !ok {
		return nil, fmt.Errorf("amount is not representable as a Decimal")
	}
	return &commonpb.Money{Amount: amt, CurrencyCode: p.baseCcy}, nil
}

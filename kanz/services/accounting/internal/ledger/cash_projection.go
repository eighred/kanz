package ledger

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
)

// CashProjection binds the portfolio and account balances to one database view
// and one economic cutoff. A future-effective deposit must not appear as venue
// collateral while it is excluded from the portfolio's cash.
type CashProjection struct {
	Book     *Book
	Accounts map[string]map[string]*big.Rat
	asOf     time.Time
	events   []*Event
}

// MaterializeCash preserves the checkpoint path for the portfolio book. The
// account projection still requires the full journal, as VenueAccountCash does.
// Inside Append it reuses the transaction so both views include the pending
// entry; outside Append it holds one repeatable-read snapshot across both reads.
func MaterializeCash(ctx context.Context, st Store, portfolio string, asOf time.Time) (CashProjection, error) {
	if asOf.IsZero() {
		return CashProjection{}, errors.New("ledger: cash projection requires an economic cutoff")
	}
	switch s := st.(type) {
	case *Postgres:
		if _, insideAppend := s.q.(pgx.Tx); insideAppend {
			return materializeCash(ctx, s, portfolio, asOf)
		}
		tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return CashProjection{}, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		projection, err := materializeCash(ctx, s.withTx(tx), portfolio, asOf)
		if err != nil {
			return CashProjection{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return CashProjection{}, err
		}
		return projection, nil
	case *MemoryStore:
		s.mu.RLock()
		defer s.mu.RUnlock()
		return materializeCash(ctx, memoryReader{m: s}, portfolio, asOf)
	case memoryReader:
		return materializeCash(ctx, s, portfolio, asOf)
	default:
		// A store without a snapshot contract cannot combine independent reads.
		events, err := st.Journal(ctx, portfolio)
		if err != nil {
			return CashProjection{}, err
		}
		return newCashProjection(ReplayAsOf(portfolio, events, asOf, time.Time{}), events, asOf), nil
	}
}

func materializeCash(ctx context.Context, st Store, portfolio string, asOf time.Time) (CashProjection, error) {
	book, _, err := materializeCurrentAt(ctx, st, portfolio, asOf)
	if err != nil {
		return CashProjection{}, err
	}
	events, err := st.Journal(ctx, portfolio)
	if err != nil {
		return CashProjection{}, err
	}
	return newCashProjection(book, events, asOf), nil
}

func newCashProjection(book *Book, events []*Event, asOf time.Time) CashProjection {
	selected := sortedFor(events, asOf, time.Time{}, true)
	kept := selected[:0]
	for _, e := range selected {
		if e.PortfolioID == "" || e.PortfolioID == book.PortfolioID {
			kept = append(kept, e)
		}
	}
	return CashProjection{Book: book, Accounts: VenueAccountCash(kept), events: kept, asOf: asOf}
}

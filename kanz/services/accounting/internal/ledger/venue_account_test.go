package ledger

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"
)

// THE ENGINE REFUSES A WRITE THAT WILL NOT SAY WHOSE COLLATERAL IT MOVES.
//
// An exchange margins and LIQUIDATES per ACCOUNT. A fill that lands in the ledger
// without its account is cash the books cannot locate — and a fill that lands in the
// WRONG account is worse: it reports collateral in a pool that does not hold it, and
// nobody finds out until a margin call in a fund that looked fine.
//
// This is enforced at the engine (app_current_venue_account() RAISES, 42501), not in
// Go, for the same reason MT-01e was: no code path — present or future, written by
// anybody — can bypass it by forgetting.

func entry(id, portfolio, account string) *Event {
	return &Event{
		EntryID:        id,
		PortfolioID:    portfolio,
		VenueAccountID: account,
		Type:           EntryTrade,
		InstrumentID:   "BTC-USD",
		Quantity:       big.NewRat(1, 1),
		Price:          big.NewRat(50000, 1),
		Cash:           big.NewRat(-50000, 1),
		CashCurrency:   "USD",
		Effective:      time.Now().UTC(),
		Knowledge:      time.Now().UTC(),
	}
}

// The account the fill settled against is recorded, and it is the account the exchange
// will liquidate.
func TestAppend_RecordsTheAccountTheFillSettledAgainst(t *testing.T) {
	pool := newPool(t)
	store := NewPostgres(pool)
	ctx := context.Background()

	if err := store.Append(ctx, entry("fill:1", "fund-alpha", "okx-alpha")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got string
	if err := pool.QueryRow(ctx,
		`SELECT venue_account_id FROM ledger_entries WHERE entry_id = 'fill:1'`).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != "okx-alpha" {
		t.Fatalf("venue_account_id = %q, want okx-alpha", got)
	}
}

// An entry that touches NO exchange account (a manual cash movement, a corporate
// action) declares that, explicitly, and is written. "I declare none" is an answer;
// "I forgot to declare" is not, and the two must not look alike.
func TestAppend_AnEntryWithNoExchangeAccountIsAllowed(t *testing.T) {
	pool := newPool(t)
	store := NewPostgres(pool)

	e := entry("cash:1", "fund-alpha", "") // no venue account: a wire in, not a fill
	e.Type = EntryCash
	e.Quantity, e.Price, e.InstrumentID = nil, nil, ""
	if err := store.Append(context.Background(), e); err != nil {
		t.Fatalf("Append of a non-exchange entry: %v — a cash movement touches no venue account "+
			"and must still be writable", err)
	}
}

// THE GUARD ITSELF. A write that does not declare its account is REFUSED by the engine
// — not defaulted, not silently attributed to whatever the last transaction used.
//
// This bypasses the store deliberately: the store always declares. The point is that
// the DATABASE refuses anyway, so a future writer that forgets cannot misfile a fill.
func TestEngine_RefusesAWriteThatDeclaresNoAccount(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
		INSERT INTO ledger_entries
			(tenant_id, entry_id, portfolio_id, entry_type, instrument_id,
			 quantity, price, cash, cash_currency, effective_time, knowledge_time)
		VALUES (current_setting('app.tenant_id'), 'fill:undeclared', 'fund-alpha', 1, 'BTC-USD',
			'1', '50000', '-50000', 'USD', now(), now())
	`)
	if err == nil {
		t.Fatal("the engine accepted a ledger entry that does not say which exchange account's " +
			"collateral it moved. A fill with no account is cash the books cannot locate, and the " +
			"whole guarantee rests on this being impossible")
	}
	if !strings.Contains(err.Error(), "app.venue_account_id") {
		t.Fatalf("refused, but not by the account guard: %v", err)
	}
}

// THE MISPOSTING. A transaction that declares one account and writes a row belonging to
// another is refused — the engine will not let fund-alpha's fill land in fund-beta's
// collateral, however the row got constructed.
func TestEngine_RefusesAFillPostedIntoAnotherAccount(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The transaction says it is moving okx-alpha's collateral...
	if _, err := tx.Exec(ctx, `SELECT set_config('app.venue_account_id', 'okx-alpha', true)`); err != nil {
		t.Fatal(err)
	}
	// ...and then writes a row into okx-BETA.
	_, err = tx.Exec(ctx, `
		INSERT INTO ledger_entries
			(tenant_id, entry_id, portfolio_id, venue_account_id, entry_type, instrument_id,
			 quantity, price, cash, cash_currency, effective_time, knowledge_time)
		VALUES (current_setting('app.tenant_id'), 'fill:misposted', 'fund-beta', 'okx-beta', 1, 'BTC-USD',
			'1', '50000', '-50000', 'USD', now(), now())
	`)
	if err == nil {
		t.Fatal("a transaction declaring okx-alpha wrote a row into okx-beta. The ledger would report " +
			"collateral in a pool that does not hold it — and a liquidation in one fund would surprise " +
			"the other")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "row-level security") &&
		!strings.Contains(strings.ToLower(err.Error()), "policy") {
		t.Fatalf("refused, but not by the RLS policy: %v", err)
	}
}

// A CASH MOVEMENT CAN NOW DECLARE AN ACCOUNT TOO (#415).
//
// The comment above this file's fixtures called a manual cash movement an entry
// that touches no exchange account, and until #415 that was forced: the cash
// producer had no field to carry one, so every funded transfer landed claiming
// it reached no exchange. Funding okx-sub-1 IS a cash movement that touches an
// exchange account, and the ledger must be able to say so.
//
// This is the round trip the unit tests cannot reach: the engine's write-guard
// (app_current_venue_account) and the RLS write policy both see a CASH entry
// carrying an account for the first time, and must treat it exactly as they
// treat a fill's.
func TestAppend_RecordsTheAccountACashMovementSettledAgainst(t *testing.T) {
	pool := newPool(t)
	store := NewPostgres(pool)
	ctx := context.Background()

	cash := &Event{
		EntryID:        "cash:S1",
		PortfolioID:    "fund-alpha",
		VenueAccountID: "okx-sub-1",
		Type:           EntryCash,
		Cash:           big.NewRat(100, 1),
		CashCurrency:   "USDT",
		Effective:      time.Now().UTC(),
		Knowledge:      time.Now().UTC(),
	}
	if err := store.Append(ctx, cash); err != nil {
		t.Fatalf("Append a funded cash movement: %v.\n"+
			"The write-guard and the RLS write policy must accept a CASH entry that declares "+
			"an account, exactly as they accept a fill's — otherwise #415's funding path is "+
			"refused by the database it was built for.", err)
	}
	var got string
	if err := pool.QueryRow(ctx,
		`SELECT venue_account_id FROM ledger_entries WHERE entry_id = 'cash:S1'`).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != "okx-sub-1" {
		t.Fatalf("venue_account_id = %q, want okx-sub-1 — the per-account index migration 0003 "+
			"built for \"what is actually in okx-sub-1\" only has cash to index if this column "+
			"is populated", got)
	}
}

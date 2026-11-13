package custody

import (
	"context"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
)

func TestPostgresCustodyReconcilesTheStatementUTCDate(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_URL to prove custody business close over a restricted-role journal and stored statement")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pg.NewTenantPool(ctx, dsn, "custody-close-acme")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var super, bypass bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatalf("restricted role required: super=%v bypass=%v err=%v", super, bypass, err)
	}
	applyCustodySchema(t, pool)
	journal := ledger.NewPostgres(pool)
	store := NewPostgres(pool)
	day := BusinessDay(t0)
	buy := closeTrade("close-buy", 10, -100, day.Add(12*time.Hour))
	for _, e := range []*ledger.Event{buy, closeTrade("close-sale", -4, 40, day.AddDate(0, 0, 1))} {
		if err := journal.Append(ctx, e, nil); err != nil {
			t.Fatal(err)
		}
	}
	stmt := statement("close-statement", map[string]int64{"AAPL": 10}, map[string]int64{"USD": -100})
	stmt.Grain = recon.GrainTransactions
	stmt.Transactions = []recon.Transaction{{ExternalRef: buy.SourceRef, InstrumentID: "AAPL", Quantity: big.NewRat(10, 1), Price: big.NewRat(10, 1), Cash: big.NewRat(-100, 1), CurrencyCode: "USD", TradeDate: buy.Effective, SettlementDate: buy.SettlementDate}}
	if err := store.SaveStatement(ctx, stmt); err != nil {
		t.Fatal(err)
	}
	for i, declared := range []bool{false, true} {
		name := "derived"
		if declared {
			name = "declared"
		}
		t.Run(name, func(t *testing.T) {
			load := LedgerBookLoader(journal, closeScope(t, declared), testLogger())
			book, executions, err := load(ctx, subject())
			if err != nil || book.Positions["AAPL"].Qty.Cmp(big.NewRat(10, 1)) != 0 || book.CashBalance("USD").Cmp(big.NewRat(-100, 1)) != 0 || len(executions) != 1 || executions[0].Ref != "close-buy" {
				t.Fatalf("D2 midnight trade leaked into D1: book=%v executions=%v err=%v", book, executions, err)
			}
			r, err := NewReconciler(store, load, &capturePublisher{}, new(big.Rat), nil, testLogger(), func() time.Time { return day.Add(time.Duration(72+i) * time.Hour) })
			if err != nil {
				t.Fatal(err)
			}
			run, err := r.Reconcile(ctx, subject())
			if err != nil || run.Outcome != OutcomeClean || len(run.Breaks) != 0 {
				t.Fatalf("D1 statement manufactured a break against D2: run=%+v err=%v", run, err)
			}
			stored, err := store.LatestRun(ctx, "PF1", "CUST-A")
			if err != nil || stored.RunID != run.RunID || stored.Outcome != OutcomeClean || !stored.Subject.BusinessDate.Equal(day) {
				t.Fatalf("dated run not durably recorded: stored=%+v err=%v", stored, err)
			}
		})
	}

	// Late-known fees restate D1 under current recorded knowledge. They do not
	// move the statement's economic date to the later transmission date.
	if err := journal.Append(ctx, &ledger.Event{EntryID: "late-close-fee", PortfolioID: "PF1", VenueAccountID: "ACC1", Type: ledger.EntryFee,
		Cash: big.NewRat(-3, 1), CashCurrency: "USD", Effective: day.Add(time.Hour), Knowledge: day.Add(72 * time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	book, executions, err := LedgerBookLoader(journal, closeScope(t, true), testLogger())(ctx, subject())
	if err != nil || book.CashBalance("USD").Cmp(big.NewRat(-103, 1)) != 0 || len(executions) != 1 {
		t.Fatalf("restated D1 omitted a late-known fee: book=%v executions=%v err=%v", book, executions, err)
	}
	otherPool, err := pg.NewTenantPool(ctx, dsn, "custody-close-other")
	if err != nil {
		t.Fatal(err)
	}
	defer otherPool.Close()
	otherBook, otherExecutions, err := LedgerBookLoader(ledger.NewPostgres(otherPool), closeScope(t, true), testLogger())(ctx, subject())
	if err != nil || len(otherBook.Positions) != 0 || len(otherBook.Cash) != 0 || len(otherExecutions) != 0 {
		t.Fatalf("another tenant discovered dated holdings: book=%v executions=%v err=%v", otherBook, otherExecutions, err)
	}
	if _, err := NewPostgres(otherPool).LatestStatement(ctx, subject()); err != ErrNoStatement {
		t.Fatalf("another tenant discovered the statement: %v", err)
	}
}

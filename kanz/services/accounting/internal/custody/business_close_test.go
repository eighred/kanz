package custody

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
)

func closeScope(t *testing.T, declared bool) *BookScope {
	t.Helper()
	var accounts map[string]map[string][]string
	if declared {
		accounts = map[string]map[string][]string{"PF1": {"CUST-A": {"ACC1"}}}
	}
	s, err := NewBookScope([]Subject{{PortfolioID: "PF1", CustodianID: "CUST-A"}}, accounts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func closeTrade(id string, qty, cash int64, at time.Time) *ledger.Event {
	return &ledger.Event{EntryID: id, PortfolioID: "PF1", VenueAccountID: "ACC1", Type: ledger.EntryTrade,
		InstrumentID: "AAPL", Quantity: big.NewRat(qty, 1), Price: big.NewRat(10, 1), Cash: big.NewRat(cash, 1), CashCurrency: "USD",
		Effective: at, Knowledge: at, SourceRef: id, SettlementBasis: ledger.SettlementSettled, SettlementDate: BusinessDay(at)}
}

func TestCustodyBookUsesTheStatementBusinessClose(t *testing.T) {
	day := BusinessDay(t0)
	for _, declared := range []bool{false, true} {
		name := "derived"
		if declared {
			name = "declared"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := ledger.NewMemoryStore()
			buy := closeTrade("day-one-buy", 10, -100, day.Add(12*time.Hour))
			for _, e := range []*ledger.Event{buy, closeTrade("day-two-sale", -4, 40, day.Add(36*time.Hour))} {
				if err := st.Append(ctx, e, nil); err != nil {
					t.Fatal(err)
				}
			}
			load := LedgerBookLoader(st, closeScope(t, declared), testLogger())
			for _, tc := range []struct {
				name       string
				date       time.Time
				qty, cash  int64
				executions int
			}{
				{"day one", day, 10, -100, 1},
				{"day two", day.AddDate(0, 0, 1), 6, -60, 2},
				{"positive offset names UTC day one", day.Add(22 * time.Hour).In(time.FixedZone("plus-three", 3*60*60)), 10, -100, 1},
				{"negative offset names UTC day two", day.Add(27 * time.Hour).In(time.FixedZone("minus-four", -4*60*60)), 6, -60, 2},
			} {
				t.Run(tc.name, func(t *testing.T) {
					book, executions, err := load(ctx, Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: tc.date})
					if err != nil {
						t.Fatal(err)
					}
					if book.Positions["AAPL"] == nil || book.Positions["AAPL"].Qty.Cmp(big.NewRat(tc.qty, 1)) != 0 || book.CashBalance("USD").Cmp(big.NewRat(tc.cash, 1)) != 0 || len(executions) != tc.executions {
						t.Fatalf("business-close book=%v cash=%v executions=%v; want qty=%d cash=%d executions=%d", book.Positions, book.CashBalance("USD"), executions, tc.qty, tc.cash, tc.executions)
					}
				})
			}

			// A real reconciliation against D1 must compare 10 shares/-100 USD,
			// with the D1 buy as its sole execution, despite the booked D2 sale.
			store := NewMemoryStore()
			stmt := statement("close-d1", map[string]int64{"AAPL": 10}, map[string]int64{"USD": -100})
			stmt.Grain = recon.GrainTransactions
			stmt.Transactions = []recon.Transaction{{ExternalRef: buy.SourceRef, InstrumentID: "AAPL", Quantity: big.NewRat(10, 1), Price: big.NewRat(10, 1), Cash: big.NewRat(-100, 1), CurrencyCode: "USD", TradeDate: buy.Effective, SettlementDate: buy.SettlementDate}}
			if err := store.SaveStatement(ctx, stmt); err != nil {
				t.Fatal(err)
			}
			r, err := NewReconciler(store, load, &capturePublisher{}, new(big.Rat), nil, testLogger(), func() time.Time { return day.Add(72 * time.Hour) })
			if err != nil {
				t.Fatal(err)
			}
			run, err := r.Reconcile(testRunContext(ctx), subject())
			if err != nil || run.Outcome != OutcomeClean || len(run.Breaks) != 0 {
				t.Fatalf("D1 reconciliation manufactured breaks from a D2 trade: run=%+v err=%v", run, err)
			}

			// This is a restated effective view using everything the journal has
			// learned now, not a historical knowledge-time reconstruction.
			correction := &ledger.Event{EntryID: "late-fee", PortfolioID: "PF1", VenueAccountID: "ACC1", Type: ledger.EntryFee,
				Cash: big.NewRat(-3, 1), CashCurrency: "USD", Effective: day.Add(time.Hour), Knowledge: day.Add(72 * time.Hour)}
			if err := st.Append(ctx, correction, nil); err != nil {
				t.Fatal(err)
			}
			book, executions, err := load(ctx, subject())
			if err != nil || book.CashBalance("USD").Cmp(big.NewRat(-103, 1)) != 0 || len(executions) != 1 {
				t.Fatalf("late-known correction missing from restated D1: book=%v executions=%v err=%v", book, executions, err)
			}
		})
	}
}

func TestCustodyBusinessCloseIncludesTheUTCDateOnly(t *testing.T) {
	day := BusinessDay(t0)
	for _, tc := range []struct {
		name      string
		effective time.Time
		want      int64
	}{
		{"start of day", day, 12},
		{"last nanosecond", day.AddDate(0, 0, 1).Add(-time.Nanosecond), 12},
		{"next midnight", day.AddDate(0, 0, 1), 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := ledger.NewMemoryStore()
			for _, e := range []*ledger.Event{closeTrade("initial", 10, -100, day.Add(-time.Hour)), closeTrade("boundary", 2, -20, tc.effective)} {
				if err := st.Append(ctx, e, nil); err != nil {
					t.Fatal(err)
				}
			}
			book, executions, err := LedgerBookLoader(st, closeScope(t, true), testLogger())(ctx, subject())
			if err != nil || book.Positions["AAPL"].Qty.Cmp(big.NewRat(tc.want, 1)) != 0 || book.CashBalance("USD").Cmp(big.NewRat(-10*tc.want, 1)) != 0 {
				t.Fatalf("boundary %s: book=%v executions=%v err=%v; want %d", tc.name, book, executions, err, tc.want)
			}
		})
	}
}

func TestCustodyBookRefusesAMissingBusinessDate(t *testing.T) {
	_, _, err := LedgerBookLoader(ledger.NewMemoryStore(), closeScope(t, true), testLogger())(context.Background(), Subject{PortfolioID: "PF1", CustodianID: "CUST-A"})
	if err == nil || !strings.Contains(err.Error(), "business_date") {
		t.Fatalf("missing business date=%v, want refusal", err)
	}
}

func TestCustodyExplicitEffectiveCutoffIsRequiredAndInclusive(t *testing.T) {
	ctx := context.Background()
	st := ledger.NewMemoryStore()
	for _, e := range []*ledger.Event{closeTrade("current", 10, -100, t0), closeTrade("future", -4, 40, t0.Add(time.Microsecond))} {
		if err := st.Append(ctx, e, nil); err != nil {
			t.Fatal(err)
		}
	}
	dateFree := Subject{PortfolioID: "PF1", CustodianID: "CUST-A"}
	book, executions, err := LedgerBookLoader(st, closeScope(t, true), testLogger(), EffectiveCutoff{AsOf: t0})(ctx, dateFree)
	if err != nil || book.Positions["AAPL"].Qty.Cmp(big.NewRat(10, 1)) != 0 || book.CashBalance("USD").Cmp(big.NewRat(-100, 1)) != 0 || len(executions) != 1 {
		t.Fatalf("explicit current cutoff failed: book=%v executions=%v err=%v", book, executions, err)
	}
	for _, tc := range []struct {
		name    string
		options []EffectiveCutoff
	}{
		{"zero", []EffectiveCutoff{{}}},
		{"multiple", []EffectiveCutoff{{AsOf: t0}, {AsOf: t0.Add(time.Hour)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Even a valid scheduled date cannot turn an invalid explicit option
			// into a silent fallback to the business close or current clock.
			_, _, err := LedgerBookLoader(st, closeScope(t, true), testLogger(), tc.options...)(ctx, subject())
			if err == nil || !strings.Contains(err.Error(), "cutoff") {
				t.Fatalf("invalid explicit cutoff=%v, want refusal", err)
			}
		})
	}
}

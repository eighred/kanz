package capital

import (
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const settleDirect = `SELECT capital_settle_direct('fund','cancel',$1,$2,$3,$4)`

func checkSettlement(t *testing.T, r releaseReader, currencies, approved, executed []string, terminal bool, want string) {
	t.Helper()
	var got string
	if err := r.QueryRow(t.Context(), settleDirect, currencies, approved, executed, terminal).Scan(&got); err != nil || got != want {
		t.Fatalf("terminal=%t, verdict=%q, want %q: %v", terminal, got, want, err)
	}
}

func TestPostgresDirectSettlementMatchesGoAcrossCashReceipt(t *testing.T) {
	for _, tc := range []struct {
		name         string
		executed     []string
		beforeCancel bool
	}{
		{"partial-then-cancel-then-book", []string{"0", "63/2", "0"}, false},
		{"partial-then-book-then-cancel", []string{"0", "63/2", "0"}, true},
		{"over-budget-and-zero-budget-leg", []string{"0.2", "402/3", "1/17"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native := database(t)
			reference := tenantPool(t, "go-reference")
			ctx := t.Context()
			for _, p := range []*pgxpool.Pool{native, reference} {
				seedRelease(t, p)
				if err := invalidateSource(ctx, p, [32]byte{1}); err != nil {
					t.Fatal(err)
				}
			}
			assertEqual := func() {
				t.Helper()
				if releaseLedger(t, native) != releaseLedger(t, reference) {
					t.Fatal("native cash, commitment or membership differs from the Go ledger")
				}
			}
			apply := func(terminal bool) {
				t.Helper()
				checkSettlement(t, native, releaseCurrencies, releaseDebits, tc.executed, terminal, "APPLIED")
				if err := transaction(reference, func(tx pgx.Tx) error {
					if err := LockBalances(ctx, tx, "fund", releaseCurrencies); err != nil {
						return err
					}
					for i, c := range releaseCurrencies {
						var err error
						if terminal {
							err = FinishExecution(ctx, tx, "fund", c, "cancel", time.Now())
						} else {
							err = AddExecution(ctx, tx, "fund", c, "cancel", dec.Exact(tc.executed[i]))
						}
						if err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				checkSettlement(t, native, releaseCurrencies, releaseDebits, tc.executed, terminal, "UNKNOWN")
				assertEqual()
			}
			book := func() {
				t.Helper()
				for i, c := range releaseCurrencies {
					if tc.executed[i] == "0" {
						continue
					}
					for _, p := range []*pgxpool.Pool{native, reference} {
						event := snapshot(2, "500")
						event.Currency, event.Complete = c, false
						event.Applied = []Applied{{OrderID: "cancel", Debit: dec.Exact(tc.executed[i])}}
						if err := Apply(ctx, p, event); err != nil {
							t.Fatal(err)
						}
						if err := Apply(ctx, p, event); err != nil {
							t.Fatal(err)
						}
					}
				}
				assertEqual()
			}
			apply(false)
			if tc.beforeCancel {
				book()
			}
			apply(true)
			if !tc.beforeCancel {
				book()
			}
		})
	}
}

func TestPostgresDirectSettlementRefusalsLeaveEveryLegIntact(t *testing.T) {
	p := database(t)
	ctx := t.Context()
	seedRelease(t, p)
	executed := []string{"0", "31.5", "0"}
	for _, tc := range []struct {
		name, sql string
		executed  []string
		terminal  bool
		want      string
	}{
		{"no-execution", "", []string{"0", "0", "0"}, false, "INVALID"},
		{"negative-leg", "", []string{"0", "-1", "0"}, false, "INVALID"},
		{"unknown-fee", "", []string{"", "31.5", "0"}, false, "INVALID"},
		{"unbound-terminal", "", executed, true, "UNKNOWN"},
		{"wrong-order-owner", `INSERT INTO capital_members(order_id,owner_order_id,currency) VALUES('child','cancel','USD')`, executed, false, "UNKNOWN"},
		{"incomplete-cash", `UPDATE capital_balances SET reserved='0' WHERE currency='USD'`, executed, false, "UNKNOWN"},
		{"unknown-allocation", `UPDATE capital_members SET allocated_debit='1' WHERE currency='ZERO' AND order_id='cancel'`, executed, false, "UNKNOWN"},
		{"unidentified-booking", `UPDATE capital_commitments SET booked_debit='1' WHERE currency='USD' AND order_id='cancel'`, executed, false, "UNKNOWN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := p.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if tc.sql != "" {
				if _, err := tx.Exec(ctx, tc.sql); err != nil {
					t.Fatal(err)
				}
			}
			before := releaseLedger(t, tx)
			checkSettlement(t, tx, releaseCurrencies, releaseDebits, tc.executed, tc.terminal, tc.want)
			if releaseLedger(t, tx) != before {
				t.Fatal("refusal changed the ledger")
			}
		})
	}
	checkSettlement(t, p, releaseCurrencies, releaseDebits, executed, false, "APPLIED")
	before := releaseLedger(t, p)
	checkSettlement(t, p, releaseCurrencies, releaseDebits, []string{"0", "30", "0"}, true, "UNKNOWN")
	if _, err := p.Exec(ctx, `CREATE FUNCTION suppress_final_leg() RETURNS trigger LANGUAGE plpgsql AS $$
        BEGIN IF NEW.currency='ZERO' THEN RETURN NULL; END IF; RETURN NEW; END $$;
        CREATE TRIGGER suppress_final_leg BEFORE UPDATE ON capital_balances FOR EACH ROW EXECUTE FUNCTION suppress_final_leg()`); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := p.QueryRow(ctx, settleDirect, releaseCurrencies, releaseDebits, executed, true).Scan(&got); err == nil {
		t.Fatal("suppressed final cash write should have aborted the transaction")
	}
	if releaseLedger(t, p) != before {
		t.Fatal("incomplete terminal transaction left a changed ledger")
	}
}

func TestPostgresConcurrentDirectSettlementAndBooking(t *testing.T) {
	p := database(t)
	seedRelease(t, p)
	ctx := t.Context()
	executed := []string{"0", "31.5", "0"}
	// Each contender uses a separate transaction and the same direct order.
	// The zero-fee leg does not change version until the terminal transition.
	contend := func(terminal bool) {
		t.Helper()
		type result struct {
			verdict string
			err     error
		}
		start := make(chan struct{})
		outcomes := make(chan result, 6)
		for range 6 {
			go func() {
				<-start
				var verdict string
				err := p.QueryRow(ctx, settleDirect, releaseCurrencies, releaseDebits, executed, terminal).Scan(&verdict)
				outcomes <- result{verdict, err}
			}()
		}
		booked := make(chan error, 1)
		if terminal {
			go func() {
				<-start
				cash := snapshot(2, "968.5")
				cash.Applied = []Applied{{OrderID: "cancel", Debit: "31.5"}}
				booked <- Apply(ctx, p, cash)
			}()
		}
		close(start)
		applied := 0
		for range 6 {
			r := <-outcomes
			if r.err != nil {
				t.Fatal(r.err)
			}
			switch r.verdict {
			case "APPLIED":
				applied++
			case "UNKNOWN":
			default:
				t.Fatalf("unexpected settlement verdict %q", r.verdict)
			}
		}
		if applied != 1 {
			t.Fatalf("terminal=%t applied %d times", terminal, applied)
		}
		if terminal {
			if err := <-booked; err != nil {
				t.Fatal(err)
			}
		}
	}
	contend(false)
	contend(true)
	var correct bool
	err := p.QueryRow(ctx, `SELECT bool_and(b.reserved='1/7' AND c.required_debit=c.executed_debit
        AND c.executed_debit=c.booked_debit AND m.executed_debit=c.executed_debit AND m.booked_debit=c.booked_debit
        AND c.version=CASE WHEN b.currency='USD' THEN 3 ELSE 2 END)
        FROM capital_balances b JOIN capital_commitments c USING(tenant_id,portfolio_id,currency)
        JOIN capital_members m USING(tenant_id,order_id,currency) WHERE c.order_id='cancel'`).Scan(&correct)
	if err != nil || !correct {
		t.Fatalf("incorrect retained liability or unrelated balance after concurrent settlement: %v", err)
	}
}

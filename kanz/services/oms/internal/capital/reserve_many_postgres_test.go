package capital

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"github.com/jackc/pgx/v5"
)

func money(currency string, amount int64) *commonpb.Money {
	return &commonpb.Money{CurrencyCode: currency, Amount: &commonpb.Decimal{Coefficient: amount}}
}

func TestPostgresReserveManyRollsBackEveryCurrencyOnRefusal(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	for _, currency := range []string{"BNB", "USDT"} {
		event := snapshot(1, "10")
		event.Currency = currency
		if err := Apply(ctx, pool, event); err != nil {
			t.Fatal(err)
		}
	}
	err := transaction(pool, func(tx pgx.Tx) error {
		err := ReserveMany(ctx, tx, "fund", "order", []*commonpb.Money{money("USDT", 11), money("BNB", 2)}, snapshot(1, "0").ObservedAt, time.Minute)
		if !errors.Is(err, ErrInsufficient) {
			return fmt.Errorf("want insufficient cash, got %v", err)
		}
		return nil // commit the outer transaction, as a persisted refusal could do
	})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM capital_commitments`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial reservation escaped: count=%d err=%v", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM capital_balances WHERE reserved <> '0'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial debit escaped: count=%d err=%v", count, err)
	}
}

func TestPostgresReserveManySerializesOppositeCurrencyOrders(t *testing.T) {
	pool := database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, currency := range []string{"BNB", "USDT"} {
		event := snapshot(1, "10")
		event.Currency = currency
		if err := Apply(ctx, pool, event); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	results := make(chan error, 12)
	start := make(chan struct{})
	for index := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tx, err := pool.Begin(ctx)
			if err != nil {
				results <- err
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			debits := []*commonpb.Money{money("USDT", 2), money("BNB", 1)}
			if index%2 == 0 {
				debits[0], debits[1] = debits[1], debits[0]
			}
			if err := ReserveMany(ctx, tx, "fund", fmt.Sprintf("order-%d", index), debits, snapshot(1, "0").ObservedAt, time.Minute); err != nil {
				results <- err
				return
			}
			results <- tx.Commit(ctx)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	admitted := 0
	for err := range results {
		if err == nil {
			admitted++
		} else if !errors.Is(err, ErrInsufficient) {
			t.Fatal(err)
		}
	}
	if admitted != 5 {
		t.Fatalf("admitted=%d, want exactly 5", admitted)
	}
	var reserved string
	for currency, want := range map[string]string{"USDT": "10", "BNB": "5"} {
		if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE currency=$1`, currency).Scan(&reserved); err != nil || reserved != want {
			t.Fatalf("%s reserved=%s want=%s err=%v", currency, reserved, want, err)
		}
	}
}

func TestPostgresExecutionAndAccountingStayInTheirCurrency(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	for _, currency := range []string{"BNB", "USDT"} {
		event := snapshot(1, "100")
		event.Currency = currency
		if err := Apply(ctx, pool, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := transaction(pool, func(tx pgx.Tx) error {
		return ReserveMany(ctx, tx, "fund", "parent", []*commonpb.Money{money("USDT", 80), money("BNB", 5)}, snapshot(1, "0").ObservedAt, time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	for _, currency := range []string{"BNB", "USDT"} {
		if err := transaction(pool, func(tx pgx.Tx) error {
			return RegisterChild(ctx, tx, "fund", currency, "parent", "child", snapshot(1, "0").ObservedAt, time.Minute)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := transaction(pool, func(tx pgx.Tx) error {
		if err := ObserveExecution(ctx, tx, "fund", "BNB", "child", "2"); err != nil {
			return err
		}
		return Change(ctx, tx, "fund", "BNB", "parent", 2, "3", snapshot(1, "0").ObservedAt, time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	booked := snapshot(2, "98")
	booked.Currency = "BNB"
	booked.Applied = []Applied{{OrderID: "child", Debit: "2"}}
	if err := Apply(ctx, pool, booked); err != nil {
		t.Fatal(err)
	}
	var required, executed, settled, reserved string
	if err := pool.QueryRow(ctx, `SELECT required_debit,executed_debit,booked_debit FROM capital_commitments WHERE order_id='parent' AND currency='USDT'`).Scan(&required, &executed, &settled); err != nil || required != "80" || executed != "0" || settled != "0" {
		t.Fatalf("fee changed quote commitment: required=%s executed=%s booked=%s err=%v", required, executed, settled, err)
	}
	if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE currency='BNB'`).Scan(&reserved); err != nil || reserved != "1" {
		t.Fatalf("fee reservation=%s err=%v", reserved, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE capital_members SET currency='USD' WHERE order_id='child' AND currency='BNB'`); err == nil {
		t.Fatal("membership currency was mutable")
	}
}

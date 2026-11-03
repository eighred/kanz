package capital

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresMemberMigrationPreservesEveryTenantDebit(t *testing.T) {
	first := databaseBefore(t, "0020_capital_members.sql")
	second := tenantPool(t, "tenant-b")
	ctx := context.Background()
	for _, pool := range []*pgxpool.Pool{first, second} {
		if err := Apply(ctx, pool, snapshot(1, "230")); err != nil {
			t.Fatal(err)
		}
		// This is the schema and retained evidence before membership existed.
		if _, err := pool.Exec(ctx, `INSERT INTO capital_commitments(order_id,portfolio_id,currency,required_debit,booked_debit,executed_debit)
			VALUES('parent','fund','USD','100','20','30'); UPDATE capital_balances SET reserved='80'`); err != nil {
			t.Fatal(err)
		}
	}
	migration, err := os.ReadFile("../../migrations/0020_capital_members.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	for _, pool := range []*pgxpool.Pool{first, second} {
		if err := transaction(pool, func(tx pgx.Tx) error {
			return RegisterChild(ctx, tx, "fund", "USD", "parent", "child", snapshot(1, "0").ObservedAt, time.Minute)
		}); err != nil {
			t.Fatal(err)
		}
		if err := transaction(pool, func(tx pgx.Tx) error {
			return ObserveExecution(ctx, tx, "fund", "USD", "parent", "35")
		}); err != nil {
			t.Fatal(err)
		}
		if err := transaction(pool, func(tx pgx.Tx) error {
			return ObserveExecution(ctx, tx, "fund", "USD", "child", "40")
		}); err != nil {
			t.Fatal(err)
		}
		event := snapshot(2, "175")
		event.Applied = []Applied{{OrderID: "parent", Debit: "35"}, {OrderID: "child", Debit: "40"}}
		if err := Apply(ctx, pool, event); err != nil {
			t.Fatal(err)
		}
		var booked, executed string
		if err := pool.QueryRow(ctx, `SELECT booked_debit,executed_debit FROM capital_commitments WHERE order_id='parent'`).Scan(&booked, &executed); err != nil || booked != "75" || executed != "75" {
			t.Fatalf("migration lost original debit: booked=%s executed=%s err=%v", booked, executed, err)
		}
		assertReserved(t, pool, "25")
	}
}

func TestPostgresScheduledMembersShareOneCommitmentAndKeepSeparateEvidence(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	now := snapshot(1, "0").ObservedAt
	if err := Apply(ctx, pool, snapshot(1, "250")); err != nil {
		t.Fatal(err)
	}
	if err := reserve(pool, "parent", "200"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"child-a", "child-b"} {
		if err := transaction(pool, func(tx pgx.Tx) error { return RegisterChild(ctx, tx, "fund", "USD", "parent", id, now, time.Minute) }); err != nil {
			t.Fatal(err)
		}
	}
	assertReserved(t, pool, "200")
	observe := func(id, debit string) {
		t.Helper()
		if err := transaction(pool, func(tx pgx.Tx) error { return ObserveExecution(ctx, tx, "fund", "USD", id, dec.Exact(debit)) }); err != nil {
			t.Fatal(err)
		}
	}
	observe("child-a", "60")
	observe("child-b", "80")
	observe("child-a", "60") // duplicate observation cannot add a second debit
	assertReserved(t, pool, "200")
	first := snapshot(2, "190")
	first.Applied = []Applied{{OrderID: "child-a", Debit: "60"}}
	if err := Apply(ctx, pool, first); err != nil {
		t.Fatal(err)
	}
	assertReserved(t, pool, "140")
	second := snapshot(3, "110")
	second.Applied = []Applied{{OrderID: "child-b", Debit: "80"}}
	if err := Apply(ctx, pool, second); err != nil {
		t.Fatal(err)
	}
	assertReserved(t, pool, "60")
	if err := transaction(pool, func(tx pgx.Tx) error { return Change(ctx, tx, "fund", "USD", "parent", 3, "140", now, time.Minute) }); err != nil {
		t.Fatal(err)
	}
	assertReserved(t, pool, "0")
	// A late fee belongs to child-a, but consumes the same parent's liability.
	observe("child-a", "65")
	assertReserved(t, pool, "5")
	fee := snapshot(4, "105")
	fee.Applied = []Applied{{OrderID: "child-a", Debit: "65"}}
	if err := Apply(ctx, pool, fee); err != nil {
		t.Fatal(err)
	}
	assertReserved(t, pool, "0")
	observe("child-a", "50")
	if err := transaction(pool, func(tx pgx.Tx) error { return Change(ctx, tx, "fund", "USD", "parent", 6, "130", now, time.Minute) }); err != nil {
		t.Fatal(err)
	}
	refund := snapshot(5, "120")
	refund.Applied = []Applied{{OrderID: "child-a", Debit: "50"}}
	if err := Apply(ctx, pool, refund); err != nil {
		t.Fatal(err)
	}
	var executed, booked string
	if err := pool.QueryRow(ctx, `SELECT executed_debit,booked_debit FROM capital_commitments WHERE order_id='parent'`).Scan(&executed, &booked); err != nil || executed != "130" || booked != "130" {
		t.Fatalf("child update replaced sibling: executed=%s booked=%s err=%v", executed, booked, err)
	}
	assertReserved(t, pool, "0")
	restarted := tenantPool(t, "tenant-a")
	if err := reserve(restarted, "new-order", "120"); err != nil {
		t.Fatalf("restart lost final available cash: %v", err)
	}
}

func TestPostgresCapitalMemberOwnershipRollbackIsolationAndQuarantine(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	now := snapshot(1, "0").ObservedAt
	if err := Apply(ctx, pool, snapshot(1, "250")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"parent-a", "parent-b"} {
		if err := reserve(pool, id, "100"); err != nil {
			t.Fatal(err)
		}
	}
	register := func(owner, child string) error {
		return transaction(pool, func(tx pgx.Tx) error { return RegisterChild(ctx, tx, "fund", "USD", owner, child, now, time.Minute) })
	}
	if err := register("parent-a", "child"); err != nil {
		t.Fatal(err)
	}
	if err := register("parent-b", "child"); !errors.Is(err, ErrConflict) {
		t.Fatalf("reassigned child: %v", err)
	}
	if err := register("child", "nested"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nested schedule ownership: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE capital_members SET owner_order_id='parent-b' WHERE order_id='child'`); err == nil {
		t.Fatal("database allowed owner reassignment")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM capital_members WHERE order_id='child'`); err == nil {
		t.Fatal("database allowed ownership evidence to be deleted")
	}
	rollback := errors.New("admission failed")
	if err := transaction(pool, func(tx pgx.Tx) error {
		if err := RegisterChild(ctx, tx, "fund", "USD", "parent-a", "rolled-back", now, time.Minute); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM capital_members WHERE order_id='rolled-back')`).Scan(&exists); err != nil || exists {
		t.Fatalf("membership escaped admission rollback: %v %v", exists, err)
	}
	other := tenantPool(t, "tenant-b")
	var count int
	if err := other.QueryRow(ctx, `SELECT count(*) FROM capital_members`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cross-tenant members: %d %v", count, err)
	}
	if err := ApplyPayload(ctx, pool, []byte{0xff}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := register("parent-a", "after-fault"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("quarantined source admitted a child: %v", err)
	}
	assertReserved(t, pool, "200")
}

func TestPostgresConcurrentChildrenDoNotOverwriteSiblingDebits(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	now := snapshot(1, "0").ObservedAt
	if err := Apply(ctx, pool, snapshot(1, "250")); err != nil {
		t.Fatal(err)
	}
	if err := reserve(pool, "parent", "200"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for index := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- transaction(pool, func(tx pgx.Tx) error {
				id := fmt.Sprintf("child-%d", index)
				if err := RegisterChild(ctx, tx, "fund", "USD", "parent", id, now, time.Minute); err != nil {
					return err
				}
				return ObserveExecution(ctx, tx, "fund", "USD", id, "10")
			})
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	booked := snapshot(2, "90")
	for index := range 16 {
		booked.Applied = append(booked.Applied, Applied{OrderID: fmt.Sprintf("child-%d", index), Debit: "10"})
	}
	if err := Apply(ctx, pool, booked); err != nil {
		t.Fatal(err)
	}
	var executed string
	if err := pool.QueryRow(ctx, `SELECT executed_debit FROM capital_commitments WHERE order_id='parent'`).Scan(&executed); err != nil || executed != "160" {
		t.Fatalf("lost concurrent execution: %s %v", executed, err)
	}
	assertReserved(t, pool, "40")
}

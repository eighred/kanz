package ledger

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type cashProjectionReadKey struct{}

type cashProjectionInterleave struct {
	once  sync.Once
	write func()
}

func (h *cashProjectionInterleave) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, cashProjectionReadKey{}, strings.Contains(data.SQL, "FROM ledger_snapshots"))
}

func (h *cashProjectionInterleave) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if selected, _ := ctx.Value(cashProjectionReadKey{}).(bool); selected {
		h.once.Do(h.write)
	}
}

func TestPostgresCashProjectionHasOneReadSnapshot(t *testing.T) {
	writerPool := newPool(t)
	writer := NewPostgres(writerPool)
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0).UTC()
	opening := cashOne("cash-view:opening", "PF-view", 250)
	opening.VenueAccountID = "account-a"
	if err := writer.Append(ctx, opening, nil); err != nil {
		t.Fatal(err)
	}
	var writeErr error
	wrote := false
	config := writerPool.Config()
	config.ConnConfig.Tracer = &cashProjectionInterleave{write: func() {
		// Commit from a different connection after the reader established its
		// snapshot, before it obtains either the journal tail or account view.
		wrote = true
		e := cashOne("cash-view:concurrent", "PF-view", 100)
		e.VenueAccountID = "account-a"
		writeErr = writer.Append(ctx, e, nil)
	}}
	readerPool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer readerPool.Close()
	projection, err := MaterializeCash(ctx, NewPostgres(readerPool), "PF-view", at)
	if err != nil || writeErr != nil || !wrote {
		t.Fatalf("projection=%v writer=%v interleaved=%v", err, writeErr, wrote)
	}
	if projection.Book.CashBalance("USD").Cmp(big.NewRat(250, 1)) != 0 || projection.Accounts["account-a"]["USD"].Cmp(big.NewRat(250, 1)) != 0 {
		t.Fatal("cash projection mixed database snapshots")
	}
	fresh, err := MaterializeCash(ctx, writer, "PF-view", at)
	if err != nil || fresh.Book.CashBalance("USD").Cmp(big.NewRat(350, 1)) != 0 || fresh.Accounts["account-a"]["USD"].Cmp(big.NewRat(350, 1)) != 0 {
		t.Fatalf("subsequent projection missed the committed deposit: %v", err)
	}
}

func TestCashProjectionEconomicCutoff(t *testing.T) {
	for _, durable := range []bool{false, true} {
		name := "memory"
		if durable {
			name = "postgres"
		}
		t.Run(name, func(t *testing.T) {
			var st Store = NewMemoryStore()
			if durable {
				pool := newPool(t)
				var bypass bool
				if err := pool.QueryRow(context.Background(), `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&bypass); err != nil || bypass {
					t.Fatalf("test requires non-bypass role: bypass=%v err=%v", bypass, err)
				}
				st = NewPostgres(pool)
			}
			ctx := context.Background()
			at := time.Unix(1_700_000_000, 0).UTC()
			first := cashOne("projection:opening", "PF-cash", 250)
			first.VenueAccountID = "account-a"
			if err := st.Append(ctx, first, nil); err != nil {
				t.Fatal(err)
			}
			// Keep a checkpoint in play: the account projection must not become
			// tail-only, and the future entry must become due at the same instant.
			book, _, err := materializeCurrentAt(ctx, st, "PF-cash", at)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SaveSnapshot(ctx, book.Snapshot(at)); err != nil {
				t.Fatal(err)
			}
			future := cashOne("projection:future", "PF-cash", 100)
			future.VenueAccountID = "account-a"
			future.Effective = at.Add(time.Hour)
			if err := st.Append(ctx, future, nil); err != nil {
				t.Fatal(err)
			}
			check := func(store Store, cutoff time.Time, want int64) error {
				projection, err := MaterializeCash(ctx, store, "PF-cash", cutoff)
				if err != nil {
					return err
				}
				if got := projection.Book.CashBalance("USD"); got.Cmp(big.NewRat(want, 1)) != 0 {
					t.Fatalf("portfolio cash=%s want=%d", got, want)
				}
				if got := projection.Accounts["account-a"]["USD"]; got == nil || got.Cmp(big.NewRat(want, 1)) != 0 {
					t.Fatalf("account cash=%v want=%d", got, want)
				}
				return nil
			}
			for _, tc := range []struct {
				cutoff time.Time
				want   int64
			}{{at, 250}, {future.Effective.Add(-time.Nanosecond), 250}, {future.Effective, 350}} {
				if err := check(st, tc.cutoff, tc.want); err != nil {
					t.Fatal(err)
				}
			}
			pending := cashOne("projection:rollback", "PF-cash", -50)
			pending.VenueAccountID = "account-a"
			boom := errors.New("refuse announcement")
			err = st.Append(ctx, pending, func(_ context.Context, txStore Store) ([]outbox.Record, error) {
				if err := check(txStore, at, 200); err != nil {
					return nil, err
				}
				return nil, boom
			})
			if !errors.Is(err, boom) {
				t.Fatalf("rollback error=%v", err)
			}
			if err := check(st, at, 250); err != nil {
				t.Fatal(err)
			}
		})
	}
}

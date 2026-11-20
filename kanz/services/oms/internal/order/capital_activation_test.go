package order

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/capital"
)

func TestPostgresCapitalActivationRefusesLegacyAndOldWriter(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		t.Run(map[bool]string{true: "legacy", false: "old-writer"}[legacy], func(t *testing.T) {
			pool := newPool(t)
			ctx := context.Background()
			now := time.Now().UTC()
			store := NewPostgres(pool)
			if legacy {
				if err := store.Create(ctx, &orderpb.OrderState{OrderId: "legacy", PortfolioId: "fund", Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund", Currency: "USD", Revision: 1, Total: "100", Complete: true, ObservedAt: now}); err != nil {
				t.Fatal(err)
			}
			st := &orderpb.OrderState{OrderId: "funded", PortfolioId: "fund", Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
			err := store.CreateFunded(ctx, st, nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(50, 0)}}, now, time.Minute)
			if legacy {
				if !errors.Is(err, ErrUnreconciledOrders) {
					t.Fatalf("legacy admission=%v", err)
				}
				var armed bool
				if err := pool.QueryRow(ctx, `SELECT enabled FROM capital_activation`).Scan(&armed); err != nil || armed {
					t.Fatalf("legacy armed=%t err=%v", armed, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var definer bool
			if err := pool.QueryRow(ctx, `SELECT prosecdef FROM pg_proc WHERE oid='require_funded_order_after_activation()'::regprocedure`).Scan(&definer); err != nil || definer {
				t.Fatalf("unexpected canonical trigger authority: definer=%t err=%v", definer, err)
			}
			err = store.Create(ctx, &orderpb.OrderState{OrderId: "unfunded", PortfolioId: "fund", Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}, nil)
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "23514" || pgerr.ConstraintName != "funded_order_activation" || pgerr.TableName != "orders" || pgerr.SchemaName != testSchema {
				t.Fatalf("expected canonical unfunded order refusal, got %v", err)
			}
			var rows int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&rows); err != nil || rows != 1 {
				t.Fatalf("orders=%d err=%v", rows, err)
			}
		})
	}
}

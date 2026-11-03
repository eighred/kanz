package order

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestFundedRecoveryRestoresExposureAfterConfirmedWithdrawal(t *testing.T) {
	pool := newPool(t)
	ctx := bus.WithTenantID(context.Background(), testTenant)
	now := time.Now().UTC()
	if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund", Currency: "USD", Revision: 1, Total: "200", Complete: true, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	store := NewPostgres(pool)
	st := &orderpb.OrderState{OrderId: "recover", PortfolioId: "fund", Venue: "SIM", VenueAccountId: "account",
		InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
		OrderedQuantity: d(2, 0), LeavesQuantity: d(2, 0), LimitPrice: d(100, 0), Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
	if err := store.CreateFunded(ctx, st, nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(200, 0)}}, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	st.Status = orderpb.OrderStatus_ORDER_STATUS_CANCELLED
	if err := store.SaveFundedTerminal(ctx, st, 0, nil, now); err != nil {
		t.Fatal(err)
	}
	fill := &orderpb.Fill{OrderId: st.OrderId, FillId: "fill", VenueExecutionId: "trade", Venue: st.Venue,
		VenueAccountId: st.VenueAccountId, InstrumentId: st.InstrumentId, Side: st.Side,
		Quantity: d(1, 0), Price: d(90, 0), Fee: &commonpb.Money{CurrencyCode: "USD", Amount: d(5, -1)}, ExecutedAt: timestamppb.New(now)}
	mapping := RecoveryMapping{testTenant, "fund", "SIM", "account", "exchange-account"}
	version, err := store.RecordRecoveryMapping(ctx, mapping, execution.AccountProof{Verified: true, ExchangeAccountID: mapping.ExchangeAccount})
	if err != nil {
		t.Fatal(err)
	}
	c, err := store.ObserveRecovery(ctx, "funded-recovery", st.OrderId, "source", []byte("complete venue history"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FreezeRecoveryHistory(ctx, c, version, st, execution.OrderView{State: execution.OrderViewCancelled,
		ExecutedQuantity: d(1, 0), Fills: []*orderpb.Fill{fill}}, now); err != nil {
		t.Fatal(err)
	}
	c, err = store.RecoveryCase(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewPostgres(pool).CommitRecovery(ctx, c, now); err != nil {
		t.Fatal(err)
	}
	var required, executed, reserved string
	if err := pool.QueryRow(ctx, `SELECT required_debit,executed_debit FROM capital_commitments WHERE order_id='recover'`).Scan(&required, &executed); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency='USD'`).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if required != "181/2" || executed != "181/2" || reserved != "181/2" {
		t.Fatalf("recovered required=%s executed=%s reserved=%s, want 90.5 until accounting inclusion", required, executed, reserved)
	}
}

package order

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRecoveryRacesNormalPushWithoutDuplicatingExecution(t *testing.T) {
	pool := newPool(t)
	s := NewPostgres(pool)
	ctx := bus.WithTenantID(context.Background(), testTenant)
	m := RecoveryMapping{testTenant, "fund", "BINANCE", "account", "exchange-account"}
	mapping, err := s.RecordRecoveryMapping(ctx, m, execution.AccountProof{Verified: true, ExchangeAccountID: m.ExchangeAccount})
	if err != nil {
		t.Fatal(err)
	}
	d := func(n int64) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: n} }
	for i := range 5 {
		id := fmt.Sprint(i)
		st := &orderpb.OrderState{OrderId: "push-" + id, PortfolioId: "fund", Venue: "BINANCE", VenueAccountId: "account", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY, OrderedQuantity: d(2), LeavesQuantity: d(2), Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED}
		if err := s.Create(ctx, st, nil); err != nil {
			t.Fatal(err)
		}
		history := &orderpb.Fill{OrderId: st.OrderId, FillId: "history-" + id, VenueExecutionId: "trade-" + id, Venue: st.Venue, VenueAccountId: st.VenueAccountId, InstrumentId: st.InstrumentId, Side: st.Side, Quantity: d(1), Price: d(100), Fee: &commonpb.Money{Amount: d(1), CurrencyCode: "USD"}, ExecutedAt: timestamppb.New(t0)}
		c, err := s.ObserveRecovery(ctx, "case-"+id, st.OrderId, "source-"+id, []byte("missing live execution"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FreezeRecoveryHistory(ctx, c, mapping, st, execution.OrderView{State: execution.OrderViewPartiallyFilled, ExecutedQuantity: d(1), Fills: []*orderpb.Fill{history}}, t0); err != nil {
			t.Fatal(err)
		}
		c, err = s.RecoveryCase(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		push := proto.Clone(history).(*orderpb.Fill)
		push.FillId = "push-alias-" + id
		next, err := ApplyFill(st, push, t0)
		if err != nil {
			t.Fatal(err)
		}
		var emitter Emitter
		record, err := emitter.FillFact(ctx, push, next)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() { <-start; results <- NewPostgres(pool).Save(ctx, next, 0, []outbox.Record{record}, push.FillId) }()
		go func() { <-start; results <- NewPostgres(pool).CommitRecovery(ctx, c, t0) }()
		close(start)
		for range 2 {
			err := <-results
			if err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrFillApplied) {
				t.Fatal(err)
			}
		}
		c, err = s.RecoveryCase(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if c.Checkpoint == 0 {
			if err := s.CommitRecovery(ctx, c, t0); err != nil {
				t.Fatal(err)
			}
		}
		actual, _, err := s.Load(ctx, st.OrderId)
		if err != nil || dec.Cmp(actual.FilledQuantity, d(1)) != 0 {
			t.Fatalf("duplicate quantity: %v %v", actual, err)
		}
		var claims, journal int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM order_fills WHERE order_id=$1),(SELECT count(*) FROM order_executions WHERE order_id=$1)`, st.OrderId).Scan(&claims, &journal); err != nil || claims != 1 || journal != 1 {
			t.Fatalf("claims=%d journal=%d err=%v", claims, journal, err)
		}
	}
}

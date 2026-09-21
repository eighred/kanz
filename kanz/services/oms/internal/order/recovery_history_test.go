package order

import (
	"context"
	"errors"
	"testing"

	"github.com/eighred/kanz/internal/execution"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRecoveryHistoryFreezesExactEconomicsWithAtomicLifecycle(t *testing.T) {
	pool := newPool(t)
	ctx := bus.WithTenantID(context.Background(), testTenant)
	s := NewPostgres(pool)
	m := RecoveryMapping{testTenant, "fund", "BINANCE", "account", "exchange-account"}
	version, err := s.RecordRecoveryMapping(ctx, m, execution.AccountProof{Verified: true, ExchangeAccountID: m.ExchangeAccount})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.ObserveRecovery(ctx, "case", "order", "event-id", []byte("aggregate evidence"))
	if err != nil {
		t.Fatal(err)
	}
	d := func(raw string) *commonpb.Decimal { v, _ := execution.ParseDec(raw); return v }
	st := &orderpb.OrderState{OrderId: c.OrderID, PortfolioId: m.Portfolio, Venue: m.Venue, VenueAccountId: m.Account,
		InstrumentId: "BTC-USD", OrderedQuantity: d("1"), Side: orderpb.Side_SIDE_BUY, Status: orderpb.OrderStatus_ORDER_STATUS_CANCELLED}
	f := &orderpb.Fill{OrderId: st.OrderId, FillId: "fill", VenueExecutionId: "trade", InstrumentId: st.InstrumentId,
		Venue: m.Venue, VenueAccountId: m.Account, Side: st.Side, Quantity: d("0.5"), Price: d("100"),
		Fee: &commonpb.Money{Amount: d("0.01"), CurrencyCode: "USD"}, ExecutedAt: timestamppb.New(t0)}
	v := execution.OrderView{State: execution.OrderViewCancelled, ExecutedQuantity: d("0.5"), Fills: []*orderpb.Fill{f}}
	// A lifecycle fact that cannot be announced must roll back its history too.
	if err := s.FreezeRecoveryHistory(context.Background(), c, version, st, v, t0); err == nil {
		t.Fatal("unpublishable history committed")
	}
	if _, _, err := s.RecoveryHistory(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.FreezeRecoveryHistory(ctx, c, version, st, v, t0); err != nil {
		t.Fatal(err)
	}
	restarted := NewPostgres(pool)
	history, digest, err := restarted.RecoveryHistory(ctx, c.ID)
	if err != nil || digest == "" || !proto.Equal(history.GetFills()[0], f) {
		t.Fatalf("history=%v digest=%s err=%v", history, digest, err)
	}
	if err := s.FreezeRecoveryHistory(ctx, c, version, st, v, t0); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale investigator: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE execution_recovery_history SET history='changed' WHERE case_id='case'`); err == nil {
		t.Fatal("frozen history changed")
	}
	current, err := restarted.RecoveryCase(ctx, c.ID)
	if err != nil || current.Status != "investigating" || current.Version != 1 || current.MappingVersion != version {
		t.Fatalf("case=%+v err=%v", current, err)
	}
	if err := restarted.BlockRecovery(ctx, current, st, "booked execution requires authorized correction", t0); err != nil {
		t.Fatal(err)
	}
	if err := restarted.BlockRecovery(ctx, current, st, "stale investigator", t0); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale block: %v", err)
	}
	current, err = restarted.RecoveryCase(ctx, c.ID)
	if err != nil || current.Status != "blocked" || current.Version != 2 {
		t.Fatalf("case=%+v err=%v", current, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE event_type=$1`, EventTypeRecoveryRecorded).Scan(&count); err != nil || count != 2 {
		t.Fatalf("lifecycle facts=%d err=%v", count, err)
	}
}

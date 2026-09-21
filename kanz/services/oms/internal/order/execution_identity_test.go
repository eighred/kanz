package order

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

func scopedClaimFacts(t *testing.T, st *orderpb.OrderState) []outbox.Record {
	t.Helper()
	records := claimFacts(st.OrderId, "BTCUSDT-7")
	f := &orderpb.Fill{FillId: "BTCUSDT-7", OrderId: st.OrderId, Venue: "BINANCE", VenueAccountId: st.VenueAccountId, InstrumentId: "BTC-USD", VenueExecutionId: "7"}
	data, err := proto.Marshal(&orderpb.OrderFilled{OrderId: st.OrderId, Fill: f, State: st})
	if err != nil {
		t.Fatal(err)
	}
	records[0].Payload = data
	return records
}

func TestScopedClaimsSeparateAccountsAndProtectRollingWriters(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	s := NewPostgres(pool)
	orders := []*orderpb.OrderState{state("account-a-order", orderpb.OrderStatus_ORDER_STATUS_FILLED), state("account-b-order", orderpb.OrderStatus_ORDER_STATUS_FILLED)}
	var records [][]outbox.Record
	for i, st := range orders {
		st.VenueAccountId = []string{"account-a", "account-b"}[i]
		if err := s.Create(ctx, st, nil); err != nil {
			t.Fatal(err)
		}
		records = append(records, scopedClaimFacts(t, st))
	}
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for i, st := range orders {
		wg.Go(func() { errorsSeen <- s.Save(ctx, st, 0, records[i], "BTCUSDT-7") })
	}
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, st := range orders {
		if err := s.Save(ctx, st, 1, records[i], "BTCUSDT-7"); !errors.Is(err, ErrFillApplied) {
			t.Fatalf("duplicate: %v", err)
		}
		claimed, err := s.AppliedFills(ctx, st.OrderId)
		if err != nil || len(claimed) != 1 || !claimed["BTCUSDT-7"] {
			t.Fatalf("aliases=%v err=%v", claimed, err)
		}
		changed := append([]outbox.Record(nil), records[i]...)
		var event orderpb.OrderFilled
		if err := proto.Unmarshal(changed[0].Payload, &event); err != nil {
			t.Fatal(err)
		}
		event.Fill.Fee = &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 1}, CurrencyCode: "USD"}
		changed[0].Payload, err = proto.Marshal(&event)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Save(ctx, st, 1, changed, "BTCUSDT-7"); !errors.Is(err, fillfact.ErrExecutionIdentityConflict) {
			t.Fatalf("fee correction silently deduplicated: %v", err)
		}
	}
	// SQL used by the previous binary must return no claim after the scoped
	// writer commits; a rolling deployment cannot double-fold that execution.
	tag, err := pool.Exec(ctx, `INSERT INTO order_fills (order_id, fill_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, orders[0].OrderId, "BTCUSDT-7")
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("legacy writer claimed again: %v %v", tag, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM order_executions`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("executions=%d err=%v", count, err)
	}
}

func TestScopedClaimDoesNotRenameLegacyBookedExecution(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	s := NewPostgres(pool)
	for _, id := range []string{"legacy-order", "another-order"} {
		st := state(id, orderpb.OrderStatus_ORDER_STATUS_FILLED)
		st.VenueAccountId = "account"
		if err := s.Create(ctx, st, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO order_fills (order_id, fill_id) VALUES ('legacy-order','BTCUSDT-7')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"legacy-order", "another-order"} {
		st, version, err := s.Load(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		err = s.Save(ctx, st, version, scopedClaimFacts(t, st), "BTCUSDT-7")
		want := ErrFillApplied
		if id == "another-order" {
			want = fillfact.ErrExecutionIdentityConflict
		}
		if !errors.Is(err, want) {
			t.Fatalf("%s: %v want %v", id, err, want)
		}
	}
}

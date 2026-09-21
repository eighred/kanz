package order

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRecoveryVerifiesExecutionsRatherThanRejectingRoundedAverage(t *testing.T) {
	pool := newPool(t)
	s := NewPostgres(pool)
	ctx := bus.WithTenantID(context.Background(), testTenant)
	d := func(raw string) *commonpb.Decimal { v, _ := execution.ParseDec(raw); return v }
	st := &orderpb.OrderState{OrderId: "rounded", PortfolioId: "fund", Venue: "BINANCE", VenueAccountId: "account", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY, OrderedQuantity: d("3"), LeavesQuantity: d("3"), Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED}
	if err := s.Create(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	var fills []*orderpb.Fill
	for i, terms := range [][2]string{{"1", "100"}, {"2", "101"}} {
		id := []string{"first", "second"}[i]
		fill := &orderpb.Fill{OrderId: st.OrderId, FillId: id, VenueExecutionId: id, Venue: st.Venue, VenueAccountId: st.VenueAccountId, InstrumentId: st.InstrumentId, Side: st.Side, Quantity: d(terms[0]), Price: d(terms[1]), Fee: &commonpb.Money{Amount: d("0"), CurrencyCode: "USD"}, ExecutedAt: timestamppb.New(t0)}
		next, err := ApplyFill(st, fill, t0)
		if err != nil {
			t.Fatal(err)
		}
		var emitter Emitter
		record, err := emitter.FillFact(ctx, fill, next)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Save(ctx, next, int64(i), []outbox.Record{record}, fill.FillId); err != nil {
			t.Fatal(err)
		}
		st = next
		fills = append(fills, fill)
	}
	if dec.FromProto(st.AverageFillPrice).Cmp(big.NewRat(302, 3)) == 0 {
		t.Fatal("fixture did not require rounding")
	}
	m := RecoveryMapping{testTenant, "fund", "BINANCE", "account", "exchange-account"}
	version, err := s.RecordRecoveryMapping(ctx, m, execution.AccountProof{Verified: true, ExchangeAccountID: m.ExchangeAccount})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.ObserveRecovery(ctx, "rounded-case", st.OrderId, "source", []byte("verify complete executions"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FreezeRecoveryHistory(ctx, c, version, st, execution.OrderView{State: execution.OrderViewFilled, ExecutedQuantity: d("3"), Fills: fills}, t0); err != nil {
		t.Fatal(err)
	}
	c, err = s.RecoveryCase(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CommitRecovery(ctx, c, t0); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Load(ctx, st.OrderId)
	want, _ := dec.ToProtoScaled(big.NewRat(302, 3))
	if err != nil || dec.Cmp(got.AverageFillPrice, want) != 0 || got.Status != st.Status {
		t.Fatalf("state=%v err=%v", got, err)
	}
	var claims int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM order_fills`).Scan(&claims); err != nil || claims != 2 {
		t.Fatalf("claims=%d err=%v", claims, err)
	}
}

func TestRecoveryCommitRetainsTerminalDispositionAndRequiresBothBooks(t *testing.T) {
	pool := newPool(t)
	ctx := bus.WithTenantID(context.Background(), testTenant)
	s := NewPostgres(pool)
	d := func(raw string) *commonpb.Decimal { v, _ := execution.ParseDec(raw); return v }
	st := &orderpb.OrderState{OrderId: "terminal", PortfolioId: "fund", Venue: "BINANCE", VenueAccountId: "account",
		InstrumentId: "BTC-USD", OrderedQuantity: d("2"), Side: orderpb.Side_SIDE_BUY, Status: orderpb.OrderStatus_ORDER_STATUS_CANCELLED}
	if err := s.Create(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	m := RecoveryMapping{testTenant, st.PortfolioId, st.Venue, st.VenueAccountId, "exchange-account"}
	mapping, err := s.RecordRecoveryMapping(ctx, m, execution.AccountProof{Verified: true, ExchangeAccountID: m.ExchangeAccount})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.ObserveRecovery(ctx, "case", st.OrderId, "observation", []byte("aggregate evidence"))
	if err != nil {
		t.Fatal(err)
	}
	f := &orderpb.Fill{OrderId: st.OrderId, FillId: "fill", VenueExecutionId: "trade", InstrumentId: st.InstrumentId,
		Venue: st.Venue, VenueAccountId: st.VenueAccountId, Side: st.Side, Quantity: d("1"), Price: d("100"),
		Fee: &commonpb.Money{Amount: d("0.1"), CurrencyCode: "USD"}, ExecutedAt: timestamppb.New(t0)}
	if err := s.FreezeRecoveryHistory(ctx, c, mapping, st, execution.OrderView{State: execution.OrderViewCancelled, ExecutedQuantity: d("1"), Fills: []*orderpb.Fill{f}}, t0); err != nil {
		t.Fatal(err)
	}
	c, err = s.RecoveryCase(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Publishing context is part of the transaction contract. A failure cannot
	// consume the execution claim or change a terminal order.
	if err := s.CommitRecovery(context.Background(), c, t0); err == nil {
		t.Fatal("unpublishable recovery committed")
	}
	var claims int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM order_fills`).Scan(&claims); err != nil || claims != 0 {
		t.Fatalf("claims=%d err=%v", claims, err)
	}
	// Two processes restarting from the same frozen evidence may race. Only one
	// may commit the economics and the other must observe the version conflict.
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { results <- NewPostgres(pool).CommitRecovery(ctx, c, t0) })
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("commits=%d", successes)
	}
	actual, _, err := s.Load(ctx, st.OrderId)
	if err != nil || actual.GetStatus() != st.Status || dec.Cmp(actual.GetFilledQuantity(), d("1")) != 0 || dec.Cmp(actual.GetAverageFillPrice(), d("100")) != 0 {
		t.Fatalf("order=%v err=%v", actual, err)
	}
	c, err = s.RecoveryCase(ctx, c.ID)
	if err != nil || c.Status != "investigating" || c.Checkpoint != 1 {
		t.Fatalf("case=%+v err=%v", c, err)
	}
	_, digest, err := s.RecoveryHistory(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	executionDigest, err := fillfact.ExecutionDigest(f)
	if err != nil {
		t.Fatal(err)
	}
	ack := &orderpb.ExecutionRecoveryApplied{CaseId: c.ID, OrderId: st.OrderId, ExecutionKey: fillfact.ExecutionKey(f), PayloadDigest: digest, ExecutionDigest: executionDigest, RecordedAt: timestamppb.New(t0)}
	wrong := proto.Clone(ack).(*orderpb.ExecutionRecoveryApplied)
	wrong.ExecutionDigest = digest + "tampered"
	if err := s.AcknowledgeRecovery(ctx, wrong, "ledger", t0); !errors.Is(err, ErrRecoveryEvidenceConflict) {
		t.Fatalf("wrong execution accepted: %v", err)
	}
	for range 2 {
		if err := s.AcknowledgeRecovery(ctx, ack, "position", t0); err != nil {
			t.Fatal(err)
		}
	}
	c, err = s.RecoveryCase(ctx, c.ID)
	if err != nil || c.Status != "investigating" {
		t.Fatalf("one book closed case: %+v %v", c, err)
	}
	if err := s.AcknowledgeRecovery(context.Background(), ack, "ledger", t0); err == nil {
		t.Fatal("unpublishable closure committed")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM execution_recovery_acks`).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("acks=%d err=%v", claims, err)
	}
	for range 2 {
		if err := NewPostgres(pool).AcknowledgeRecovery(ctx, ack, "ledger", t0); err != nil {
			t.Fatal(err)
		}
	}
	c, err = s.RecoveryCase(ctx, c.ID)
	if err != nil || c.Status != "corrected" {
		t.Fatalf("case=%+v err=%v", c, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM order_fills`).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("claims=%d err=%v", claims, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE event_type=$1`, EventTypeRecoveryRecorded).Scan(&claims); err != nil || claims != 3 {
		t.Fatalf("lifecycle=%d err=%v", claims, err)
	}
}

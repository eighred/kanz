package projection

import (
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/fillfact"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	tvsyncpb "github.com/eighred/kanz/kanz-schemas-go/tvsync/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func venueAccountExecution(orderID, venueAccount string, quantity int64) *orderpb.OrderFilled {
	event := filledOrder(orderID, "fund", "BTC-USD", orderpb.Side_SIDE_BUY, quantity, 100)
	event.State.Venue, event.State.VenueAccountId = "BINANCE", venueAccount
	f := event.Fill
	f.FillId, f.VenueExecutionId = "BTCUSDT-7", "7"
	f.Venue, f.VenueAccountId = "BINANCE", venueAccount
	f.Fee = &commonpb.Money{Amount: d(1), CurrencyCode: "USDT"}
	f.ExecutedAt = timestamppb.New(time.Unix(1800000000, 123000000).UTC())
	return event
}

func TestVenueAccountExecutionsRestart(t *testing.T) { venueAccountExecutionsRestart(t, newMemLog()) }
func TestVenueAccountExecutionsPostgresRestart(t *testing.T) {
	pool := newPool(t, testTenant)
	freshSchema(t, pool)
	venueAccountExecutionsRestart(t, NewPostgresLog(pool))
}

func venueAccountExecutionsRestart(t *testing.T, log Log) {
	t.Helper()
	ctx := t.Context()
	live := New(time.Now, nil, WithLog(log, testTenant))
	first, second := venueAccountExecution("first", "book-A", 2), venueAccountExecution("second", "book-B", 3)
	// Earlier raw Binance fills lacked venue_account_id. Only this original
	// order's retained terms bind the old fill to book-A.
	old := proto.Clone(first).(*orderpb.OrderFilled)
	old.Fill.VenueAccountId = ""
	deliver(t, live, "legacy-first", evtFilled, old)
	if err := live.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	wire, _, _, err := log.LoadCheckpoint(ctx)
	var cp tvsyncpb.Checkpoint
	if err != nil || proto.Unmarshal(wire, &cp) != nil || cp.ExactEncodingVersion != 2 || cp.Accounts[0].Execs[0].GetSourceFill().GetVenueAccountId() != "book-A" {
		t.Fatal("checkpoint did not retain verified account or version", err)
	}
	restart := func() *Projection {
		p := New(time.Now, nil, WithLog(log, testTenant))
		if err := p.Rehydrate(ctx); err != nil {
			t.Fatal(err)
		}
		return p
	}
	live = restart()
	deliver(t, live, "echo-A", evtFilled, first)
	deliver(t, live, "second", evtFilled, second)
	if err := live.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	live = restart()
	deliver(t, live, "echo-B", evtFilled, second)
	deliver(t, live, "second", evtFilled, second)
	got, err := live.Executions(testTenant, "fund", "", time.Time{})
	if err != nil || len(got) != 2 || got[0].VenueFillID != "BTCUSDT-7" || got[1].VenueFillID != "BTCUSDT-7" || got[0].FillID == got[1].FillID || got[0].FillID != fillfact.ExecutionKey(first.Fill) || got[1].FillID != fillfact.ExecutionKey(second.Fill) || got[0].OrderID == got[1].OrderID {
		t.Fatalf("lookalike trade IDs did not yield two distinct orders: %+v %v", got, err)
	}
	pos, err := live.Positions(testTenant, "fund", time.Time{})
	if err != nil || len(pos) != 1 || pos[0].Qty != "5" || pos[0].AvgPrice != "100" {
		t.Fatalf("position after redelivery: %+v %v", pos, err)
	}
	conflict := proto.Clone(first).(*orderpb.OrderFilled)
	conflict.Fill.Fee.CurrencyCode = "BNB"
	before := live.seq
	if err := live.Handle(ctx, factEnv("conflict", evtFilled), mustMarshal(t, conflict)); !errors.Is(err, fillfact.ErrExecutionIdentityConflict) || live.seq != before {
		t.Fatalf("fee conflict advanced fold: %v", err)
	}
	if err := New(time.Now, nil, WithLog(log, testTenant)).Rehydrate(ctx); !errors.Is(err, fillfact.ErrExecutionIdentityConflict) {
		t.Fatal("restart accepted conflicting durable tail", err)
	}
}

func TestVenueAccountLegacyCheckpointRequiresSourceProof(t *testing.T) {
	ctx := t.Context()
	log := newMemLog()
	p := New(time.Now, nil, WithLog(log, testTenant))
	first := venueAccountExecution("order-A", "account-A", 2)
	old := proto.Clone(first).(*orderpb.OrderFilled)
	old.Fill.VenueAccountId = ""
	deliver(t, p, "original", evtFilled, old)
	if err := p.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	// The original v1 binary retained no execution ID, venue account or fee currency.
	var legacy tvsyncpb.Checkpoint
	if err := proto.Unmarshal(log.cp, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.ExactEncodingVersion = 1
	legacy.Accounts[0].Execs[0].SourceFill = nil
	legacy.Accounts[0].SeenFills = []string{first.Fill.FillId}
	log.cp = mustMarshal(t, &legacy)
	restored := New(time.Now, nil, WithLog(log, testTenant))
	if err := restored.Rehydrate(ctx); err != nil {
		t.Fatal(err)
	}
	before := restored.seq
	if err := restored.Handle(ctx, factEnv("accounted-echo", evtFilled), mustMarshal(t, first)); !errors.Is(err, ErrUnresolvedFillAlias) || restored.seq != before || before != 1 {
		t.Fatalf("identity-free snapshot unexpectedly advanced: seq=%d err=%v", restored.seq, err)
	}
	if err := restored.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if log.cpSeq != 1 || restored.Resident().Executions != 1 {
		t.Fatal("ambiguous alias crossed the folded watermark")
	}
	// Independently replay both original raw facts into an empty projection.
	// Here the timestamp, fee, venue execution ID and bound order account survive.
	replay := New(time.Now, nil)
	if err := log.Replay(ctx, 0, func(f Fact) error { _, err := replay.fold(testTenant, f.EventType, f.Payload, f.Knowledge); return err }); err != nil {
		t.Fatal(err)
	}
	if replay.Resident().Executions != 1 {
		t.Fatal("provable legacy trade was counted twice during full replay")
	}
}

func TestVenueAccountAmbiguousLegacyRawFacts(t *testing.T) {
	for _, legacyFirst := range []bool{true, false} {
		log := newMemLog()
		p := New(time.Now, nil, WithLog(log, testTenant))
		known := venueAccountExecution("order-A", "account-A", 2)
		unknown := proto.Clone(known).(*orderpb.OrderFilled)
		unknown.State.VenueAccountId = ""
		unknown.Fill.VenueAccountId = ""
		first, second := known, unknown
		if legacyFirst {
			first, second = unknown, known
		}
		deliver(t, p, "first", evtFilled, first)
		if err := p.Handle(t.Context(), factEnv("second", evtFilled), mustMarshal(t, second)); !errors.Is(err, ErrUnresolvedFillAlias) || p.seq != 1 || p.Resident().Executions != 1 {
			t.Fatal("unbound account overlap advanced fold", err)
		}
	}
}

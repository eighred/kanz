package projection

import (
	"context"
	"errors"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	tvsyncpb "github.com/eighred/kanz/kanz-schemas-go/tvsync/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func roundedReplayFixture(t *testing.T) (*memLog, replaySnapshot) {
	return roundedReplayFixtureAt(t, time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC))
}

func roundedReplayFixtureAt(t *testing.T, now time.Time) (*memLog, replaySnapshot) {
	t.Helper()
	log := newMemLog()
	p := New(func() time.Time { return now }, nil, WithLog(log, testTenant), WithRetention(7*24*time.Hour))
	ev := filledOrder("tiny", "fund", "BTC", orderpb.Side_SIDE_BUY, 1, 100)
	ev.Fill.Quantity = &commonpb.Decimal{Coefficient: 1, Exponent: -10}
	ev.State.OrderedQuantity = proto.Clone(ev.Fill.Quantity).(*commonpb.Decimal)
	ev.State.FilledQuantity = proto.Clone(ev.Fill.Quantity).(*commonpb.Decimal)
	ev.State.AsOf = timestamppb.New(now)
	ev.Fill.ExecutedAt = timestamppb.New(now)
	deliver(t, p, "event-tiny", evtFilled, ev)
	if err := p.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Deliberately corrupt a newly encoded checkpoint to retain the independent
	// numerical-mismatch control after the production encoder is repaired.
	var cp tvsyncpb.Checkpoint
	if err := proto.Unmarshal(log.cp, &cp); err != nil {
		t.Fatal(err)
	}
	cp.Accounts[0].Execs[0].Qty = &commonpb.Decimal{}
	log.cp = mustMarshal(t, &cp)
	return log, replaySnapshot{at: now, count: 1, watermark: 1, checkpointSeq: log.cpSeq, checkpoint: log.cp, identity: "rounded-synthetic-snapshot"}
}

func TestReplayVerifierDoesNotRoundAwayMismatch(t *testing.T) {
	log, snap := roundedReplayFixture(t)
	// A lossy checkpoint claiming exact encoding must still fail numerical
	// comparison; the format marker alone is not proof of correct state.
	var cp tvsyncpb.Checkpoint
	if err := proto.Unmarshal(snap.checkpoint, &cp); err != nil {
		t.Fatal(err)
	}
	if cp.Accounts[0].Execs[0].Qty.Coefficient != 0 {
		t.Fatal("fixture did not exercise checkpoint rounding")
	}
	if _, err := verifyCapturedReplay(context.Background(), log, testTenant, 7*24*time.Hour, snap); !errors.Is(err, ErrReplayMismatch) {
		t.Fatalf("rounded mismatch qualified: %v", err)
	}
}

func replayFixture(t *testing.T) (*memLog, replaySnapshot) {
	t.Helper()
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	log := newMemLog()
	p := New(func() time.Time { return now }, nil, WithLog(log, testTenant), WithRetention(7*24*time.Hour))
	add := func(id string, side orderpb.Side, qty, price int64) {
		ev := filledOrder(id, "fund", "BTC", side, qty, price)
		ev.State.AsOf = timestamppb.New(now)
		ev.Fill.ExecutedAt = timestamppb.New(now)
		deliver(t, p, "event-"+id, evtFilled, ev)
	}
	add("one", orderpb.Side_SIDE_BUY, 2, 100)
	add("two", orderpb.Side_SIDE_BUY, 3, 110)
	if err := p.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	add("three", orderpb.Side_SIDE_SELL, 1, 150)
	// Independent arithmetic: 5 at average106, sell1 at150 -> realized44, 4 remain.
	positions, err := p.Positions(testTenant, "fund", time.Time{})
	if err != nil || len(positions) != 1 || positions[0].Qty != "4" || positions[0].AvgPrice != "106" {
		t.Fatalf("independent position oracle: %+v %v", positions, err)
	}
	state, err := p.State(testTenant, "fund", time.Time{})
	if err != nil || state.RealizedPnl != "44" {
		t.Fatal("independent realized-PnL oracle failed")
	}
	return log, replaySnapshot{at: now, count: 3, watermark: 3, checkpointSeq: log.cpSeq, checkpoint: log.cp, identity: "synthetic-snapshot"}
}

func TestReplayVerifierCanonicalCheckpointAndTail(t *testing.T) {
	log, snap := replayFixture(t)
	beforeCP := append([]byte(nil), log.cp...)
	proof, err := verifyCapturedReplay(context.Background(), log, testTenant, 7*24*time.Hour, snap)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Status != "VERIFIED" || proof.FactCount != 3 || proof.CheckpointSeq != 2 || proof.Watermark != 3 || len(proof.StateSHA256) != 64 {
		t.Fatalf("bad binding: %+v", proof)
	}
	if string(beforeCP) != string(log.cp) || len(log.facts) != 3 {
		t.Fatal("verification wrote history")
	}
	ro := readOnlyReplay{}
	if _, _, err := ro.Append(context.Background(), Fact{}); !errors.Is(err, ErrReplayReadOnly) {
		t.Fatal("append allowed")
	}
	if err := ro.SaveCheckpoint(context.Background(), 1, nil); !errors.Is(err, ErrReplayReadOnly) {
		t.Fatal("checkpoint write allowed")
	}
}

func TestReplayVerifierRefusesUnsupportedAndCorruptState(t *testing.T) {
	for _, name := range []string{"missing_checkpoint", "wrong_tenant", "wrong_sequence", "duplicate_account", "unknown_checkpoint", "unknown_fact", "unknown_event", "corrupt_state", "future_knowledge", "missing_history", "cancelled", "empty_revisions", "exponent_rational", "missing_fill_price"} {
		t.Run(name, func(t *testing.T) {
			log, snap := replayFixture(t)
			var cp tvsyncpb.Checkpoint
			if err := proto.Unmarshal(snap.checkpoint, &cp); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			switch name {
			case "missing_checkpoint":
				snap.checkpoint = nil
			case "wrong_tenant":
				cp.Accounts[0].TenantId = "other"
			case "wrong_sequence":
				cp.Seq++
			case "duplicate_account":
				cp.Accounts = append(cp.Accounts, proto.Clone(cp.Accounts[0]).(*tvsyncpb.AccountSnapshot))
			case "unknown_checkpoint":
				cp.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 1})
			case "unknown_fact":
				log.facts[0].Payload = append(log.facts[0].Payload, 0xf8, 0x07, 1)
			case "unknown_event":
				log.facts[0].EventType = "unsupported"
			case "corrupt_state":
				cp.Accounts[0].Execs[0].Qty = d(99)
			case "future_knowledge":
				log.facts[0].Knowledge = snap.at.Add(time.Hour)
			case "missing_history":
				log.facts = log.facts[1:]
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "empty_revisions":
				cp.Accounts[0].Orders["one"] = &tvsyncpb.OrderRevisions{}
			case "exponent_rational":
				cp.Accounts[0].Baseline = map[string]*tvsyncpb.Lot{"BTC": {Qty: "1e999999999", AvgCost: "0", Realized: "0"}}
			case "missing_fill_price":
				var ev orderpb.OrderFilled
				if err := proto.Unmarshal(log.facts[0].Payload, &ev); err != nil {
					t.Fatal(err)
				}
				ev.Fill.Price = nil
				log.facts[0].Payload = mustMarshal(t, &ev)
			}
			if name != "missing_checkpoint" {
				snap.checkpoint = mustMarshal(t, &cp)
			}
			if _, err := verifyCapturedReplay(ctx, log, testTenant, 7*24*time.Hour, snap); err == nil {
				t.Fatal("unsafe snapshot qualified")
			}
		})
	}
}
